package runs_test

// Reports end to end (docs/REPORTS.md): a player files over real HTTP with a
// session and an Origin, a moderator works the queue behind real permission
// gates, and the action a resolution refers to happens on its own surface.
//
// This suite is also what pins the ADMIN MOUNT ORDER. /admin/reports and
// /admin/quotes are sibling mounts under a ban surface mounted on "/", and if
// that root mount is registered first its catch-all swallows both. Nothing but
// a request through the real router catches that.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// admin promotes an account to the admin role. The role is read per request, so
// this takes effect on the very next call with that session.
func (h *harness) admin(userID string) {
	h.t.Helper()
	_, err := h.pool.Exec(h.t.Context(),
		`UPDATE users SET role = 'admin' WHERE id = $1`, userID)
	require.NoError(h.t, err)
}

// quoteFor plants one published quote and returns its id.
func (h *harness) quoteFor(text string) uuid.UUID {
	h.t.Helper()
	var id uuid.UUID
	require.NoError(h.t, h.pool.QueryRow(h.t.Context(),
		`INSERT INTO quotes (id, lang, upstream_id, text, source, length, len_group, text_hash)
		 VALUES (gen_random_uuid(), 'english', 4242, $1, 'test', $2, 0, md5($1))
		 RETURNING id`, text, len(text)).Scan(&id))
	return id
}

type queueResp struct {
	Items []struct {
		Subject struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"subject"`
		OpenReports int64    `json:"openReports"`
		Reasons     []string `json:"reasons"`
		Snapshot    struct {
			UserName       string `json:"userName"`
			QuoteText      string `json:"quoteText"`
			QuoteWithdrawn bool   `json:"quoteWithdrawn"`
			RunOwnerName   string `json:"runOwnerName"`
		} `json:"snapshot"`
	} `json:"items"`
}

// The whole loop: a player reports a quote, a moderator sees it grouped in the
// queue with a snapshot, withdraws the quote on its own surface, and records
// the verdict on the report — two calls, because a report records a decision
// and never performs one.
func TestReportQueueLoop(t *testing.T) {
	h := newHarness(t)
	quoteID := h.quoteFor("a thoroughly offensive quote")

	h.login("reporter@example.com", "correct horse battery", "reporterone")
	requireStatus(t, h.post("/api/v1/reports", map[string]any{
		"subject": map[string]string{"type": "quote", "id": quoteID.String()},
		"reason":  "offensive",
		"comment": "not ok",
	}), http.StatusCreated)

	// A second player raises the same thing: two reports, still one queue item.
	h.logout()
	h.login("second@example.com", "correct horse battery", "reportertwo")
	requireStatus(t, h.post("/api/v1/reports", map[string]any{
		"subject": map[string]string{"type": "quote", "id": quoteID.String()},
		"reason":  "wrong_language",
	}), http.StatusCreated)

	// The same player again is the same report, not a second one.
	requireStatus(t, h.post("/api/v1/reports", map[string]any{
		"subject": map[string]string{"type": "quote", "id": quoteID.String()},
		"reason":  "offensive",
	}), http.StatusOK)

	// A plain player cannot see the queue: the admin subtree does not exist to
	// them (404, not 403).
	requireStatus(t, h.get("/api/v1/admin/reports"), http.StatusNotFound)

	adminID := h.login("mod@example.com", "correct horse battery", "themoderator")
	h.admin(adminID)

	queue := decodeInto[queueResp](t, h.get("/api/v1/admin/reports"))
	require.Len(t, queue.Items, 1, "two reports, one subject, one queue item")
	item := queue.Items[0]
	assert.Equal(t, "quote", item.Subject.Type)
	assert.Equal(t, quoteID.String(), item.Subject.ID)
	assert.EqualValues(t, 2, item.OpenReports)
	assert.Equal(t, []string{"offensive", "wrong_language"}, item.Reasons)
	assert.Equal(t, "a thoroughly offensive quote", item.Snapshot.QuoteText,
		"the snapshot is what makes the queue triageable without opening items")
	assert.False(t, item.Snapshot.QuoteWithdrawn)

	// The ACTION, on the quote surface — the report system never does this
	// itself.
	withdrawn := decodeInto[struct {
		Changed    bool `json:"changed"`
		Withdrawal struct {
			Withdrawn bool   `json:"withdrawn"`
			Reason    string `json:"reason"`
		} `json:"withdrawal"`
	}](t, h.post("/api/v1/admin/quotes/"+quoteID.String()+"/withdrawal",
		map[string]string{"reason": "offensive text"}))
	assert.True(t, withdrawn.Changed)
	assert.True(t, withdrawn.Withdrawal.Withdrawn)

	// Then the verdict, which closes the whole group at once.
	resolved := decodeInto[struct {
		Status   string `json:"status"`
		Resolved int64  `json:"resolved"`
	}](t, h.post("/api/v1/admin/reports/quote/"+quoteID.String()+"/resolve",
		map[string]string{"verdict": "actioned", "note": "withdrew it"}))
	assert.Equal(t, "actioned", resolved.Status)
	assert.EqualValues(t, 2, resolved.Resolved)

	queue = decodeInto[queueResp](t, h.get("/api/v1/admin/reports"))
	assert.Empty(t, queue.Items, "the queue is clear")

	// The history stays, with the verdict on every report.
	detail := decodeInto[struct {
		Reports []struct {
			Status         string `json:"status"`
			ReporterName   string `json:"reporterName"`
			ResolverName   string `json:"resolverName"`
			ResolutionNote string `json:"resolutionNote"`
		} `json:"reports"`
	}](t, h.get("/api/v1/admin/reports/quote/"+quoteID.String()))
	require.Len(t, detail.Reports, 2)
	for _, rep := range detail.Reports {
		assert.Equal(t, "actioned", rep.Status)
		assert.Equal(t, "themoderator", rep.ResolverName)
		assert.Equal(t, "withdrew it", rep.ResolutionNote)
		assert.NotEmpty(t, rep.ReporterName, "moderators see who filed")
	}
}

// The admin subtrees share a prefix: /admin/reports and /admin/quotes are
// sibling mounts under a ban surface mounted at the root of /admin. All three
// must answer, which only holds because the root mount is registered last.
func TestAdminSubtreesCoexist(t *testing.T) {
	h := newHarness(t)
	quoteID := h.quoteFor("some quote text")
	adminID := h.login("both@example.com", "correct horse battery", "adminboth")
	h.admin(adminID)

	requireStatus(t, h.get("/api/v1/admin/bans"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/admin/reports"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/admin/quotes/"+quoteID.String()), http.StatusOK)
	requireStatus(t, h.get("/api/v1/admin/users/adminboth/bans"), http.StatusOK)
}

// Filing is gated: a session is required, and a banned account may not file at
// all — the queue is for signal from players in good standing.
func TestFilingIsGated(t *testing.T) {
	h := newHarness(t)
	quoteID := h.quoteFor("a quote to report")
	body := map[string]any{
		"subject": map[string]string{"type": "quote", "id": quoteID.String()},
		"reason":  "typo",
	}

	requireStatus(t, h.post("/api/v1/reports", body), http.StatusUnauthorized)

	userID := h.login("gated@example.com", "correct horse battery", "gateduser")
	requireStatus(t, h.post("/api/v1/reports", body), http.StatusCreated)

	h.ban(userID)
	// A banned player's existing report stands; they simply cannot file more.
	requireStatus(t, h.post("/api/v1/reports", map[string]any{
		"subject": map[string]string{"type": "quote", "id": h.quoteFor("another").String()},
		"reason":  "typo",
	}), http.StatusForbidden)
}

// The reason vocabulary is per subject type, and the pairing is refused at the
// edge with a 400 rather than reaching the database as a constraint violation.
func TestReportValidation(t *testing.T) {
	h := newHarness(t)
	quoteID := h.quoteFor("valid quote")
	h.login("valid@example.com", "correct horse battery", "validuser")

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"a reason belonging to another subject type", map[string]any{
			"subject": map[string]string{"type": "quote", "id": quoteID.String()},
			"reason":  "offensive_name",
		}, http.StatusBadRequest},
		{"an unknown subject type", map[string]any{
			"subject": map[string]string{"type": "room", "id": quoteID.String()},
			"reason":  "other",
		}, http.StatusBadRequest},
		{"a subject id that is not a uuid", map[string]any{
			"subject": map[string]string{"type": "quote", "id": "nope"},
			"reason":  "typo",
		}, http.StatusBadRequest},
		{"a subject that does not exist", map[string]any{
			"subject": map[string]string{"type": "quote", "id": uuid.New().String()},
			"reason":  "typo",
		}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireStatus(t, h.post("/api/v1/reports", tc.body), tc.want)
		})
	}
}

// A run report carries the replay a moderator needs to judge it: the queue's
// snapshot names the owner and the run's status.
func TestRunReportsCarryTheirOwner(t *testing.T) {
	h := newHarness(t)
	h.login("cheat@example.com", "correct horse battery", "suspectrun")
	resp := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")))
	h.replayOnce(t)

	h.logout()
	h.login("watcher@example.com", "correct horse battery", "thewatcher")
	requireStatus(t, h.post("/api/v1/reports", map[string]any{
		"subject": map[string]string{"type": "run", "id": resp.ID},
		"reason":  "impossible_score",
	}), http.StatusCreated)

	adminID := h.login("runmod@example.com", "correct horse battery", "therunmod")
	h.admin(adminID)
	queue := decodeInto[queueResp](t, h.get("/api/v1/admin/reports?type=run"))
	require.Len(t, queue.Items, 1)
	assert.Equal(t, "run", queue.Items[0].Subject.Type)
	assert.Equal(t, resp.ID, queue.Items[0].Subject.ID)
	assert.Equal(t, "suspectrun", queue.Items[0].Snapshot.RunOwnerName,
		"a run report has to say whose run it is")
}

// A withdrawn quote's board leaves the INDEX and nothing else. The boundary is
// the one the design turns on: withdrawal stops a quote being offered, it does
// not un-earn a result somebody already played for, and it can never make a
// stored run unreplayable.
func TestWithdrawnQuoteLeavesTheBoardIndexOnly(t *testing.T) {
	h := newHarness(t)
	h.login("boardq@example.com", "correct horse battery", "boardquoter")

	v := goldenVector(t, "quote-fixed-text")
	require.NotNil(t, v.Quote)
	h.publishQuote(t, v.Quote.ID, v.Quote.Text, v.Quote.Hash, false)
	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", v.Payload))
	h.replayOnce(t)

	bucket := "quote:" + v.Quote.ID.String()
	require.Len(t, bucketIndex(t, h), 1, "the quote's board exists before withdrawal")
	require.Len(t, boardPage(t, h, bucket), 1)

	adminID := h.login("boardmod@example.com", "correct horse battery", "boardmod")
	h.admin(adminID)
	requireStatus(t, h.post("/api/v1/admin/quotes/"+v.Quote.ID.String()+"/withdrawal",
		map[string]string{"reason": "offensive"}), http.StatusOK)

	// Gone from discovery.
	assert.Empty(t, bucketIndex(t, h), "a withdrawn quote's board leaves the index")

	// Still there on its own URL, with the entry intact — the run was
	// legitimately played and its rank is not a moderator's to take.
	assert.Len(t, boardPage(t, h, bucket), 1,
		"a direct link to the board still answers")

	// And the run itself is untouched: still accepted, and still WATCHABLE by
	// anyone — the public replay is the strongest form of "withdrawal cannot
	// make history unreplayable", since it resolves the withdrawn quote's bytes
	// to render the playback.
	requireStatus(t, h.get("/api/v1/runs/"+ingested.ID+"/replay"), http.StatusOK)
	h.loginAs("boardq@example.com", "correct horse battery")
	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	assert.Equal(t, "accepted", after["status"])

	h.loginAs("boardmod@example.com", "correct horse battery")

	// Restoring puts the board back in the index — nothing was destroyed.
	requireStatus(t, h.del("/api/v1/admin/quotes/"+v.Quote.ID.String()+"/withdrawal"),
		http.StatusOK)
	assert.Len(t, bucketIndex(t, h), 1)
}
