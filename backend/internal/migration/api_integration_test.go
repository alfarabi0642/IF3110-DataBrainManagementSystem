package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/google/uuid"
)

func (f draftHTTPFixture) jsonRequest(t *testing.T, method, path string, session draftSession, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if session.token != "" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.token})
	}
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", f.origin)
		r.Header.Set("X-CSRF-Token", session.csrf)
	}
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != want || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Location") == "/login" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: %d %.250s", method, path, w.Code, w.Body.String())
	}
	for _, private := range []string{session.token, "password_sealed", "postgres://", "databrain_target", "PRIVATE"} {
		if private != "" && strings.Contains(w.Body.String(), private) {
			t.Fatal("private data in JSON response")
		}
	}
	return w
}

func apiData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var envelope struct {
		Data      T      `json:"data"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.RequestID == "" {
		t.Fatal("invalid JSON envelope")
	}
	return envelope.Data
}

func jsonBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMigrationAPIPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18085")
	ctx := context.Background()
	base := "/api/databases/" + f.a.String()
	drafts := base + "/drafts"
	create := func(sql string) draftDTO {
		return apiData[struct {
			Draft draftDTO `json:"draft"`
		}](t, web.jsonRequest(t, "POST", drafts, web.editor, jsonBody(t, map[string]string{"title": "猫 <script>draft</script>", "sql": sql}), 201)).Draft
	}
	input := func(revision, key string) string {
		return jsonBody(t, map[string]string{"expected_revision": revision, "request_key": key})
	}
	code := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Error.Code != want {
			t.Fatalf("expected %s, got %s", want, w.Body.String())
		}
	}
	d := create("\r\nNOT SQL <script> 猫;\t")
	path := drafts + "/" + d.ID
	if d.Revision != "1" || d.SQL == nil || *d.SQL != "\r\nNOT SQL <script> 猫;\t" {
		t.Fatal("invalid draft SQL was normalized or rejected")
	}
	for _, tc := range []struct {
		session draftSession
		status  int
	}{{web.viewer, 403}, {web.none, 404}, {web.admin, 404}, {web.anonymous, 401}, {draftSession{}, 401}} {
		web.jsonRequest(t, "GET", drafts, tc.session, "", tc.status)
		web.jsonRequest(t, "POST", path+"/execute", tc.session, input("1", uuid.NewString()), tc.status)
	}
	for _, body := range []string{`{"title":"x","sql":"x","actor":"forged"}`, `{"title":"x","title":"y","sql":"x"}`, `{"title":"x","sql":null}`} {
		web.jsonRequest(t, "POST", drafts, web.editor, body, 400)
	}
	web.jsonRequest(t, "POST", drafts, web.editor, `{"title":" ","sql":"x"}`, 422)
	noCSRF := web.editor
	noCSRF.csrf = "wrong"
	web.jsonRequest(t, "POST", drafts, noCSRF, `{"title":"x","sql":"x"}`, 403)
	r := httptest.NewRequest("POST", drafts, strings.NewReader(`{"title":"x","sql":"x"}`))
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: web.editor.token})
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", web.editor.csrf)
	r.Header.Set("Origin", "http://foreign.example")
	w := httptest.NewRecorder()
	web.app.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	key := uuid.NewString()
	code(web.jsonRequest(t, "POST", path+"/execute", web.editor, input("1", key), 422), "SQL_INVALID")
	unsupported := create("SELECT 1;")
	code(web.jsonRequest(t, "POST", drafts+"/"+unsupported.ID+"/execute", web.editor, input("1", uuid.NewString()), 422), "SQL_UNSUPPORTED")
	sql := "\r\n-- 猫; exact snapshot\r\nCREATE TABLE public.api_once(id int PRIMARY KEY,value text); INSERT INTO public.api_once VALUES(1,'a;b');"
	save := map[string]string{"title": "Saved snapshot", "sql": sql, "expected_revision": "1"}
	saved := apiData[struct {
		Draft draftDTO `json:"draft"`
	}](t, web.jsonRequest(t, "POST", path, web.editor, jsonBody(t, save), 200)).Draft
	if saved.Revision != "2" {
		t.Fatal("revision not advanced")
	}
	code(web.jsonRequest(t, "POST", path, web.editor, jsonBody(t, save), 409), "REVISION_CONFLICT")
	current := apiData[struct {
		Draft draftDTO `json:"draft"`
	}](t, web.jsonRequest(t, "GET", path, web.editor, "", 200)).Draft
	if current.SQL == nil || *current.SQL != sql || current.Revision != "2" {
		t.Fatal("conflict overwrote draft")
	}
	review := apiData[struct {
		Draft    draftDTO `json:"draft"`
		Revision string   `json:"expected_revision"`
		Key      string   `json:"request_key"`
	}](t, web.jsonRequest(t, "GET", path+"/review", web.editor, "", 200))
	if review.Revision != "2" || review.Key == "" || review.Draft.SQL == nil || *review.Draft.SQL != sql {
		t.Fatal("review binding missing")
	}
	canonical := path + "/review?expected_revision=2&request_key=" + review.Key
	web.jsonRequest(t, "GET", canonical, web.editor, "", 200)
	var attempts int
	var absent bool
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_attempts`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("read or invalid execution reserved attempt")
	}
	if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.api_once') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("review executed SQL")
	}
	receipt := apiData[receiptDTO](t, web.jsonRequest(t, "POST", path+"/execute", web.editor, input("2", review.Key), 200))
	if receipt.State != "succeeded" || receipt.RequestKey != review.Key {
		t.Fatal("missing committed receipt")
	}
	var count int
	if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.api_once`).Scan(&count); err != nil || count != 1 {
		t.Fatal("target did not commit")
	}
	save["expected_revision"], save["sql"] = "2", "invalid latest SQL"
	web.jsonRequest(t, "POST", path, web.editor, jsonBody(t, save), 200)
	for range 2 {
		replayed := apiData[receiptDTO](t, web.jsonRequest(t, "POST", path+"/execute", web.editor, input("2", review.Key), 200))
		if replayed != receipt {
			t.Fatal("same key replay changed receipt")
		}
		web.jsonRequest(t, "GET", canonical, web.editor, "", 200)
	}
	code(web.jsonRequest(t, "POST", path+"/execute", web.editor, input("3", review.Key), 409), "IDEMPOTENCY_CONFLICT")
	code(web.jsonRequest(t, "POST", path+"/execute", web.editor, input("2", uuid.NewString()), 409), "REVISION_CONFLICT")
	web.jsonRequest(t, "GET", base+"/receipts/"+review.Key, web.otherEditor, "", 404)
	web.jsonRequest(t, "GET", base+"/receipts/"+review.Key, web.editor, "", 200)
	detail := apiData[struct {
		Attempt attemptDTO `json:"attempt"`
	}](t, web.jsonRequest(t, "GET", base+"/attempts/"+receipt.ID, web.viewer, "", 200)).Attempt
	hash := sha256.Sum256([]byte(sql))
	if detail.SQL == nil || *detail.SQL != sql || detail.SQLHash != hex.EncodeToString(hash[:]) || detail.Title != "Saved snapshot" || detail.Revision != "2" || detail.Actor != "draft_editor_a" {
		t.Fatal("history did not preserve snapshots")
	}
	web.jsonRequest(t, "GET", "/api/databases/"+f.b.String()+"/attempts/"+receipt.ID, web.viewer, "", 404)
	history := web.jsonRequest(t, "GET", base+"/history?limit=1", web.viewer, "", 200)
	if strings.Contains(history.Body.String(), "request_key") || strings.Contains(history.Body.String(), "sql_hash") {
		t.Fatal("history list exposed detail")
	}
	failed := create("CREATE TABLE public.api_rolled_back(id int); INSERT INTO public.api_once VALUES(1,'duplicate');")
	failure := apiData[receiptDTO](t, web.jsonRequest(t, "POST", drafts+"/"+failed.ID+"/execute", web.editor, input("1", uuid.NewString()), 200))
	if failure.State != "failed" {
		t.Fatal("definite failure not recorded")
	}
	if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.api_rolled_back') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("target transaction did not roll back")
	}
	failedDetail := apiData[struct {
		Attempt attemptDTO `json:"attempt"`
	}](t, web.jsonRequest(t, "GET", base+"/attempts/"+failure.ID, web.viewer, "", 200)).Attempt
	if failedDetail.Failure == nil || failedDetail.Failure.SQLState != "23505" || failedDetail.Failure.StatementIndex != 2 {
		t.Fatal("failure evidence missing")
	}
	runnerSQL(t, f.schema, `CREATE FUNCTION databrain.api_reject_success() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='succeeded' THEN RAISE EXCEPTION 'PRIVATE'; END IF; RETURN NEW; END $$;
CREATE TRIGGER api_reject_success BEFORE UPDATE ON databrain.migration_attempts FOR EACH ROW EXECUTE FUNCTION databrain.api_reject_success();`)
	defer runnerSQL(t, f.schema, `DROP TRIGGER api_reject_success ON databrain.migration_attempts; DROP FUNCTION databrain.api_reject_success()`)
	unknown := create("CREATE TABLE public.api_unknown(id int); INSERT INTO public.api_unknown VALUES(1)")
	unknownKey := uuid.NewString()
	unknownPath := drafts + "/" + unknown.ID + "/execute"
	uncertainReceipt := apiData[receiptDTO](t, web.jsonRequest(t, "POST", unknownPath, web.editor, input("1", unknownKey), 200))
	if uncertainReceipt.State != "unknown" {
		t.Fatal("unrecorded commit advertised as terminal")
	}
	web.jsonRequest(t, "POST", unknownPath, web.editor, input("1", unknownKey), 200)
	code(web.jsonRequest(t, "POST", unknownPath, web.editor, input("1", uuid.NewString()), 409), "TARGET_BUSY")
	if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.api_unknown`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unknown replay repeated committed SQL")
	}
	unknownDetail := apiData[struct {
		Attempt attemptDTO `json:"attempt"`
	}](t, web.jsonRequest(t, "GET", base+"/attempts/"+uncertainReceipt.ID, web.viewer, "", 200)).Attempt
	if unknownDetail.State != "unknown" || unknownDetail.FinishedAt != nil || unknownDetail.FirstUnknownAt == nil || unknownDetail.Failure == nil {
		t.Fatal("unknown history misrepresented evidence")
	}
}

func TestMigrationAPIConcurrentPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18085")
	ctx := context.Background()
	d := attemptDraft(t, f.draftFixture, f.a, `UPDATE public.draft_marker SET value=value||';json-once'`)
	key := uuid.New()
	base := "/api/databases/" + f.a.String()
	path := base + "/drafts/" + d.ID.String() + "/execute"
	body := jsonBody(t, map[string]string{"expected_revision": "1", "request_key": key.String()})
	lock, err := f.target.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackAttempt(lock)
	runnerSQL(t, lock, `UPDATE public.draft_marker SET value=value`)
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: web.editor.token})
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", web.origin)
	r.Header.Set("X-CSRF-Token", web.editor.csrf)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); web.app.ServeHTTP(w, r); result <- w }()
	done := false
	defer func() {
		rollbackAttempt(lock)
		if !done {
			<-result
		}
	}()
	active := waitRunnerTarget(t, f, f.a, key, "Lock")
	replay := apiData[receiptDTO](t, web.jsonRequest(t, "POST", path, web.editor, body, 200))
	if replay.ID != active.ID.String() || replay.State != "running" {
		t.Fatal("concurrent replay did not return running receipt")
	}
	web.jsonRequest(t, "GET", base+"/receipts/"+key.String(), web.editor, "", 200)
	web.jsonRequest(t, "GET", base+"/attempts/"+replay.ID, web.viewer, "", 200)
	busy := web.jsonRequest(t, "POST", path, web.editor, jsonBody(t, map[string]string{"expected_revision": "1", "request_key": uuid.NewString()}), 409)
	if !strings.Contains(busy.Body.String(), `"code":"TARGET_BUSY"`) {
		t.Fatal("same-target contention was not rejected")
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	completed := <-result
	done = true
	if completed.Code != 200 || apiData[receiptDTO](t, completed).State != "succeeded" {
		t.Fatal("execution owner did not return committed receipt")
	}
	var value string
	if err := f.target.QueryRow(ctx, `SELECT value FROM public.draft_marker`).Scan(&value); err != nil || value != "unchanged 猫;json-once" {
		t.Fatal("concurrent JSON replay repeated SQL")
	}
}
