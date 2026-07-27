package ws

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"time"

	"github.com/typemore/typemore-server/internal/protocol"
)

// snapshotLocked copies everything persistence needs so the gzip/marshal work
// (and the store call) can happen off the room lock. The captured batch slices
// are immutable once appended, so sharing their backing arrays is safe.
func (r *Room) snapshotLocked(m *matchState) matchSnapshot {
	snap := matchSnapshot{
		id:        m.id,
		roomCode:  r.code,
		name:      m.settings.Name,
		settings:  m.settings,
		players:   m.players,
		seed:      m.seed,
		dictHash:  m.settings.DictHash,
		lang:      m.settings.Lang,
		goAtMs:    m.goAtMs,
		endedAtMs: nowMs(),
	}
	for _, s := range m.roster {
		status := s.status
		if status == seatActive {
			status = protocol.StatusDNF // defensive; should not happen
		}
		snap.runs = append(snap.runs, runSnapshot{
			playerID: s.playerID,
			nick:     s.nick,
			userID:   s.userID,
			freemods: s.freemods,
			status:   status,
			batches:  s.batches,
		})
	}
	return snap
}

type runSnapshot struct {
	playerID string
	nick     string
	userID   string
	freemods protocol.Freemods
	status   string
	batches  []CapturedBatch
}

type matchSnapshot struct {
	id        string
	roomCode  string
	name      string
	settings  protocol.Settings
	players   []protocol.CountdownPlayer
	seed      int64
	dictHash  string
	lang      string
	goAtMs    int64
	endedAtMs int64
	runs      []runSnapshot
}

// persist gzips each run's capture and writes the whole match in one store call.
// It runs off the room lock; a failure is logged (the capture is best-effort in
// v0, and the room has already returned to the lobby).
func (r *Room) persist(snap matchSnapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	settingsJSON, err := json.Marshal(snap.settings)
	if err != nil {
		r.log.Error("marshal match settings", "matchId", snap.id, "err", err)
		return
	}
	freemodsJSON, err := json.Marshal(snap.players)
	if err != nil {
		r.log.Error("marshal match freemods", "matchId", snap.id, "err", err)
		return
	}
	rec := MatchRecord{
		ID:       snap.id,
		RoomCode: snap.roomCode,
		Name:     snap.name,
		Settings: settingsJSON,
		Freemods: freemodsJSON,
		Seed:     snap.seed,
		DictHash: snap.dictHash,
		Lang:     snap.lang,
		GoAt:     time.UnixMilli(snap.goAtMs),
		EndedAt:  time.UnixMilli(snap.endedAtMs),
	}
	for _, run := range snap.runs {
		logBytes, gzErr := gzipBatches(run.batches)
		if gzErr != nil {
			r.log.Error("gzip capture", "matchId", snap.id, "player", run.playerID, "err", gzErr)
			return
		}
		fmJSON, fmErr := json.Marshal(run.freemods)
		if fmErr != nil {
			r.log.Error("marshal run freemods", "matchId", snap.id, "err", fmErr)
			return
		}
		rec.Runs = append(rec.Runs, MatchRunRecord{
			PlayerID:    run.playerID,
			Nick:        run.nick,
			UserID:      run.userID,
			Freemods:    fmJSON,
			Log:         logBytes,
			BatchCount:  len(run.batches),
			FinalStatus: run.status,
		})
	}
	if err := r.store.SaveMatch(ctx, rec); err != nil {
		r.log.Error("persist match", "matchId", snap.id, "err", err)
	}
}

// gzipBatches marshals the capture to JSON and gzip-compresses it.
func gzipBatches(batches []CapturedBatch) ([]byte, error) {
	if batches == nil {
		batches = []CapturedBatch{}
	}
	raw, err := json.Marshal(batches)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
