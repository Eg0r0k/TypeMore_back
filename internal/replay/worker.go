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

// Worker turns pending runs into accepted/flagged/rejected ones.
//
// It owns no state beyond its dependencies: each goroutine claims a batch,
// replays every run in it through its own goja runtime, and commits the
// verdicts in the transaction that claimed them.
type Worker struct {
	queue Queue
	reg   *Registry
	cfg   WorkerConfig
	log   *slog.Logger
}

// NewWorker builds the worker. The registry supplies dictionary bodies by hash;
// the queue supplies (and persists) the work.
func NewWorker(q Queue, reg *Registry, cfg WorkerConfig, log *slog.Logger) *Worker {
	return &Worker{queue: q, reg: reg, cfg: cfg.withDefaults(), log: log}
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
		d := Judge(ctx, core, w.reg, w.cfg.Policy, run)
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
func Judge(ctx context.Context, core *Core, reg *Registry, policy Policy, run PendingRun) Decision {
	body, ok := reg.Body(run.DictHash)
	if !ok {
		return policy.Decide(run, Result{}, ErrUnknownDict)
	}

	logJSON, err := gunzip(run.Log)
	if err != nil {
		return policy.Decide(run, Result{}, fmt.Errorf("replay: decompress log: %w", err))
	}

	res, err := core.Replay(ctx, Input{
		Seed:         run.Seed,
		DictHash:     run.DictHash,
		DictBody:     body,
		Setup:        run.Setup,
		Log:          logJSON,
		ScoreVersion: run.ScoreVersion,
	})
	return policy.Decide(run, res, err)
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
