package ws_test

// A ban closes the door to NEW rooms (docs/MODERATION.md, reversal of
// 2026-08-06): create_room/join_room answer account_restricted, checked per
// action so a mid-session ban bites without evicting anybody.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// restrictedServer treats X-Test-User as the identity and X-Test-Restricted
// as the ban lookup's answer — resolved per ACTION, off the header captured at
// upgrade, which is exactly how a live lookup consults the database each time.
func restrictedServer(t *testing.T, restricted func(userID string) bool) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := chi.NewRouter()
	r.Handle("/ws", ws.NewHandler(logger, nil, func(req *http.Request) (string, string, bool) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			return name, "uid-" + name, true
		}
		return "", "", false
	}, nil).WithRestrictions(func(_ context.Context, userID string) bool {
		return restricted(userID)
	}))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestRestrictedAccountCannotCreateOrJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	banned := map[string]bool{"uid-cheater": true}
	srv := restrictedServer(t, func(userID string) bool { return banned[userID] })

	// The banned account: create_room is refused, the connection stays open.
	c := dialAs(t, ctx, srv, "cheater")
	doHello(t, ctx, c, "cheater")
	writeJSON(t, ctx, c, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	e := decodeErr(t, expect(t, ctx, c, protocol.TypeError))
	assert.Equal(t, protocol.CodeAccountRestricted, e.Code)

	// An honest host opens a room…
	host := dialAs(t, ctx, srv, "honest")
	_, st := hostRoom(t, ctx, host)

	// …and the banned account cannot follow them in either.
	writeJSON(t, ctx, c, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: st.Code})
	e = decodeErr(t, expect(t, ctx, c, protocol.TypeError))
	assert.Equal(t, protocol.CodeAccountRestricted, e.Code)

	// A guest (no account) is not what bans restrict.
	guest := dialAs(t, ctx, srv, "")
	joinRoom(t, ctx, guest, st.Code, host)
}

func TestBanLandingMidSessionBitesOnTheNextRoomAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	banned := map[string]bool{}
	srv := restrictedServer(t, func(userID string) bool { return banned[userID] })

	// Before the ban the account rooms freely.
	c := dialAs(t, ctx, srv, "soon-banned")
	_, st := hostRoom(t, ctx, c)
	_ = st

	// The ban lands; the seat they HOLD is untouched, but leaving and coming
	// back is a new room action and is refused. (The read loop is sequential,
	// so the leave is fully processed before the create below is dispatched.)
	banned["uid-soon-banned"] = true
	writeJSON(t, ctx, c, protocol.Leave{Type: protocol.TypeLeave})

	writeJSON(t, ctx, c, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	e := decodeErr(t, expect(t, ctx, c, protocol.TypeError))
	assert.Equal(t, protocol.CodeAccountRestricted, e.Code)
}
