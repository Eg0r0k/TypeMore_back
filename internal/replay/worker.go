package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Worker configuration defaults. They are deliberately modest: replay is
// background work that must never compete with request serving.
const (
	DefaultPollInterval = 2 * time.Second
	DefaultBatchSize    = 20
	DefaultConcurrency  = 1
	// maxLogBytes caps the decompressed event log. The ingest layer already
	// caps the compressed submission at 2 MB; this is the gzip-bomb guard on
	// the way back out.
	maxLogBytes = 16 << 20
)

// WorkerConfig is the replay worker's tuning surface. Every field has a working
// default, so the zero value is a valid single-threaded worker.
type WorkerConfig struct {
	// PollInterval is how long to wait after an EMPTY batch before scanning
	// again. A full batch is followed immediately by the next one, so a backlog
	// drains at full speed.
	PollInterval time.Duration
	// BatchSize is how many runs one transaction claims.
	BatchSize int32
	// Concurrency is the number of independent workers. Each gets its own goja
	// runtime; they share the queue through FOR UPDATE SKIP LOCKED.
	Concurrency int
	// ReplayTimeout bounds a single core call.
	ReplayTimeout time.Duration
	// ShutdownGrace bounds how long an in-flight batch may take to finish after
	// the shutdown signal. The batch runs on an uncancelled context so it can
	// commit its work rather than roll it back.
	ShutdownGrace time.Duration
	// Policy is the flag-scoring rule set. The zero value means DefaultPolicy.
	Policy Policy
}

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.ReplayTimeout <= 0 {
		c.ReplayTimeout = DefaultReplayTimeout
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.Policy.Version == 0 {
		c.Policy = DefaultPolicy()
	}
	return c
}

// QuoteResolver reads back the fixed text a quote run was played on.
//
// Declared HERE, at the consumer, exactly as Projector and DictVersioner are
// declared at theirs. This package owns the vendored bundle that the quote
// domain hashes text with; importing that domain back would close a cycle for
// the sake of two strings. The composition root knows both sides and wires
// them (internal/quote.ReplayResolver is the adapter).
//
// The (value, ok, err) shape mirrors Registry.Body deliberately: "no such
// quote" and "the registry could not answer" are different decisions. The
// first flags the run; the second retries it.
type QuoteResolver interface {
	// ResolveQuote returns a quote's text and its registry text_hash by id,
	// SUPERSEDED REVISIONS INCLUDED. A run played on a since-retired revision
	// must keep replaying forever, so a resolver that hid them would make
	// every such run permanently unjudgeable.
	ResolveQuote(ctx context.Context, id uuid.UUID) (text, textHash string, ok bool, err error)
}

// Worker turns pending runs into accepted/flagged/rejected ones.
//
// It owns no state beyond its dependencies: each goroutine claims a batch,
// replays every run in it through its own goja runtime, and commits the
// verdicts in the transaction that claimed them.
type Worker struct {
	queue  Queue
	reg    *Registry
	quotes QuoteResolver
	cfg    WorkerConfig
	log    *slog.Logger
}

// NewWorker builds the worker. The registry supplies dictionary bodies by hash
// for seeded runs, the quote resolver supplies fixed texts by id for quote
// runs, and the queue supplies (and persists) the work.
func NewWorker(q Queue, reg *Registry, quotes QuoteResolver, cfg WorkerConfig, log *slog.Logger) *Worker {
	return &Worker{queue: q, reg: reg, quotes: quotes, cfg: cfg.withDefaults(), log: log}
}

// Run blocks until ctx is cancelled, then returns once every in-flight batch has
// finished. Each goroutine builds its own Core up front: a broken bundle is a
// startup failure of the worker, not a per-run surprise.
func (w *Worker) Run(ctx context.Context) error {
	cores := make([]*Core, w.cfg.Concurrency)
	for i := range cores {
		core, err := NewCore(w.cfg.ReplayTimeout)
		if err != nil {
			return fmt.Errorf("replay: build worker core: %w", err)
		}
		cores[i] = core
	}

	w.log.Info("replay worker starting",
		"concurrency", w.cfg.Concurrency,
		"batchSize", w.cfg.BatchSize,
		"pollInterval", w.cfg.PollInterval,
		"replayTimeout", w.cfg.ReplayTimeout,
		"bundleSha", bundleSHA[:12],
		"policyVersion", w.cfg.Policy.Version,
		"reviewThreshold", w.cfg.Policy.ReviewThreshold,
	)

	var wg sync.WaitGroup
	for i, core := range cores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, core, w.log.With("worker", i))
		}()
	}
	wg.Wait()
	w.log.Info("replay worker stopped")
	return nil
}

// loop is one worker goroutine: drain the queue, sleep when it is empty, exit on
// shutdown. A batch already started is always allowed to finish.
func (w *Worker) loop(ctx context.Context, core *Core, log *slog.Logger) {
	timer := time.NewTimer(w.cfg.PollInterval)
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		// The batch does NOT inherit cancellation: once rows are claimed we
		// want the verdicts committed, not rolled back on the way out. The
		// grace timeout is what stops that from being unbounded.
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.ShutdownGrace)
		claimed, err := w.RunBatch(batchCtx, core, log)
		cancel()

		switch {
		case err != nil:
			log.ErrorContext(ctx, "replay batch failed", "err", err)
		case claimed == int(w.cfg.BatchSize):
			// A full batch probably means a backlog: go straight round again.
			continue
		}

		// Empty queue, partial batch, or an error: back off before rescanning.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.cfg.PollInterval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// batchTally is the per-batch outcome summary.
type batchTally struct {
	accepted int
	flagged  int
	rejected int
	// failed counts runs whose replay itself failed (timeout / core error).
	// They are also counted in `flagged` — the status they land in.
	failed int
}

// RunBatch claims and processes one batch of PENDING runs. Exported so tests
// can drive exactly one pass without racing a poll loop.
func (w *Worker) RunBatch(ctx context.Context, core *Core, log *slog.Logger) (int, error) {
	return w.runBatch(ctx, core, log, "replay batch done", func(decide func(context.Context, PendingRun) Decision) (int, error) {
		return w.queue.ProcessBatch(ctx, w.cfg.BatchSize, decide)
	})
}

// RevalidateBatch re-judges one batch of runs that are no longer current on
// either axis: policy_version behind CurrentPolicyVersion, or bundle_sha
// different from the vendored bundle's. Same judging path as the queue — the
// numbers are recomputed from the log, not read back — so a bundle change is
// re-judged with the code that now disagrees with the row.
//
// bundleSHA is the SAME value Policy.Decide stamps onto every decision, so
// "what the claim looks for" and "what the apply writes" cannot drift into two
// digests and revalidate the same rows forever.
//
// Idempotent: a run it touches stops matching both arms of the claim.
func (w *Worker) RevalidateBatch(ctx context.Context, core *Core, log *slog.Logger) (int, error) {
	return w.runBatch(ctx, core, log, "revalidate batch done", func(decide func(context.Context, PendingRun) Decision) (int, error) {
		return w.queue.ProcessStalePolicyBatch(ctx, w.cfg.Policy.Version, bundleSHA, w.cfg.BatchSize, decide)
	})
}

func (w *Worker) runBatch(
	ctx context.Context,
	core *Core,
	log *slog.Logger,
	msg string,
	process func(func(context.Context, PendingRun) Decision) (int, error),
) (int, error) {
	var tally batchTally
	started := time.Now()

	claimed, err := process(func(ctx context.Context, run PendingRun) Decision {
		d := Judge(ctx, core, w.reg, w.quotes, w.cfg.Policy, run)
		switch d.Status {
		case StatusAccepted:
			tally.accepted++
		case StatusRejected:
			tally.rejected++
		default:
			tally.flagged++
		}
		if d.LastError != "" && d.Attempts > run.Attempts {
			tally.failed++
		}
		return d
	})
	if err != nil {
		return 0, err
	}
	if claimed > 0 {
		log.InfoContext(ctx, msg,
			"claimed", claimed,
			"accepted", tally.accepted,
			"flagged", tally.flagged,
			"rejected", tally.rejected,
			"failed", tally.failed,
			"tookMs", time.Since(started).Milliseconds(),
		)
	}
	return claimed, nil
}

// Judge replays one run and maps the outcome onto a decision under the given
// policy. It never returns an error: every failure mode is a decision, which is
// what keeps one bad run from wedging the queue.
//
// Exported because the worker is not the only caller — `replayctl calibrate`
// judges runs without writing, and it has to reach the verdict by exactly the
// same route or its report would be a fiction.
//
// Where a run's TEXT comes from is decided here, once, before the core is
// entered. A seeded run gets its dictionary body from the registry; a quote run
// gets its bytes from the quote registry and never touches the dictionary
// registry at all — its dict_hash is dictVersion([text]) and would not resolve
// there, so a shared lookup would reject every quote run before it began.
func Judge(ctx context.Context, core *Core, reg *Registry, quotes QuoteResolver, policy Policy, run PendingRun) Decision {
	in := Input{
		Seed:         run.Seed,
		DictHash:     run.DictHash,
		Setup:        run.Setup,
		ScoreVersion: run.ScoreVersion,
	}

	ref, isQuote, err := quoteRefOf(run.Setup)
	if err != nil {
		return policy.Decide(run, Result{}, err)
	}
	if isQuote {
		quote, err := resolveQuote(ctx, quotes, ref)
		if err != nil {
			return policy.Decide(run, Result{}, err)
		}
		in.Quote = quote
	} else {
		body, ok := reg.Body(run.DictHash)
		if !ok {
			return policy.Decide(run, Result{}, ErrUnknownDict)
		}
		in.DictBody = body
	}

	if in.Log, err = gunzip(run.Log); err != nil {
		return policy.Decide(run, Result{}, fmt.Errorf("replay: decompress log: %w", err))
	}

	res, err := core.Replay(ctx, in)
	return policy.Decide(run, res, err)
}

// quoteRef is a run's own claim about which fixed text it was typed against.
type quoteRef struct {
	ID   uuid.UUID
	Hash string
}

// textSourceEnvelope is the sliver of the setup snapshot the worker has to
// read. Everything else stays opaque and reaches the core verbatim.
type textSourceEnvelope struct {
	Generation struct {
		TextSource *struct {
			Kind      string `json:"kind"`
			QuoteID   string `json:"quoteId"`
			QuoteHash string `json:"quoteHash"`
		} `json:"textSource"`
	} `json:"generation"`
}

// quoteRefOf reports the quote a run was played on.
//
// ok is false for a seeded run, INCLUDING the legacy shape where textSource is
// absent entirely. Absent means seeded — that is the whole reason quotes did
// not have to bump EVENT_LOG_VERSION — so nothing here may require the key.
func quoteRefOf(setup json.RawMessage) (quoteRef, bool, error) {
	var env textSourceEnvelope
	if err := json.Unmarshal(setup, &env); err != nil {
		return quoteRef{}, false, fmt.Errorf("replay: setup.generation is unreadable: %w", err)
	}
	src := env.Generation.TextSource
	if src == nil || src.Kind != TextSourceQuote {
		return quoteRef{}, false, nil
	}
	id, err := uuid.Parse(src.QuoteID)
	if err != nil {
		return quoteRef{}, false, fmt.Errorf("%w: quoteId %q is not a uuid", ErrUnknownQuote, src.QuoteID)
	}
	return quoteRef{ID: id, Hash: src.QuoteHash}, true, nil
}

// resolveQuote reads a run's fixed text back out of the registry and checks it
// is the text the run claims. Returns the bytes the core will regenerate the
// targets from — never the client's copy, which is refused at ingestion and
// would be overwritten here regardless.
func resolveQuote(ctx context.Context, quotes QuoteResolver, ref quoteRef) (*QuoteText, error) {
	if quotes == nil {
		return nil, fmt.Errorf("%w: this worker has no quote registry wired in", ErrUnknownQuote)
	}
	text, hash, ok, err := quotes.ResolveQuote(ctx, ref.ID)
	switch {
	case err != nil:
		// The registry failed to ANSWER, which is not the registry answering
		// "no such quote". A database blip must be retried, not converted into
		// a verdict, so this falls through to the generic replay_error arm and
		// increments attempts.
		return nil, fmt.Errorf("replay: resolve quote %s: %w", ref.ID, err)
	case !ok:
		return nil, fmt.Errorf("%w: no quote %s in the registry", ErrUnknownQuote, ref.ID)
	case hash != ref.Hash:
		// The corpus is the likelier thing to have moved. A re-import never
		// edits a published quote — it inserts a new revision beside it — so a
		// hash that no longer matches says something about which bytes this
		// node holds, not that the player typed a different text. Flagged, so
		// a human or a later `make revalidate` decides; rejecting would spend
		// the one verdict that cannot be walked back on a guess.
		return nil, fmt.Errorf("%w: quote %s hashes to %s, the run claims %s",
			ErrUnknownQuote, ref.ID, hash, ref.Hash)
	}
	return &QuoteText{Text: text, Hash: hash}, nil
}

// gunzip decompresses a stored event log, refusing anything absurdly large.
func gunzip(gz []byte) (json.RawMessage, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()

	raw, err := io.ReadAll(io.LimitReader(zr, maxLogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxLogBytes {
		return nil, errors.New("decompressed log exceeds the size limit")
	}
	return raw, nil
}
