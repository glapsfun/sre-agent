package ledger

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/store"
)

func openLedger(t *testing.T, root string) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, func() time.Time { return ledgerTime }), s
}

func startLedger(t *testing.T, s *Service, id, session string) Run {
	t.Helper()
	run, err := s.Start(t.Context(), StartRequest{ID: id, Problem: "checkout failure", SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func addLedger(t *testing.T, s *Service, runID, id string, kind Kind, payload any) Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Add(t.Context(), AddRequest{RunID: runID, ID: id, Kind: kind, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func ledgerInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var value int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func hypothesis(ref string) Hypothesis {
	return Hypothesis{Statement: "cause", Mechanism: "mechanism", Confidence: "high", Supporting: []string{ref}, Contradicting: []string{}, DiscriminatingTest: "compare", Status: "open"}
}

func TestAddRejectsUnknownCrossRunAndWrongKindEvidence(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	startLedger(t, s, "one", "")
	startLedger(t, s, "two", "")
	addLedger(t, s, "two", "other-evidence", KindEvidence, Evidence{Fact: "fact", Source: "query"})
	addLedger(t, s, "one", "metadata", KindMetadata, map[string]any{"phase": 1})
	for i, ref := range []string{"missing", "other-evidence", "metadata", "self"} {
		id := fmt.Sprintf("invalid-%d", i)
		if ref == "self" {
			id = ref
		}
		raw, _ := json.Marshal(hypothesis(ref))
		_, err := s.Add(t.Context(), AddRequest{RunID: "one", ID: id, Kind: KindHypothesis, Payload: raw})
		if !errors.Is(err, ErrInvalidReference) {
			t.Fatalf("reference %s accepted or wrong error: %v", ref, err)
		}
	}
	if got := ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries WHERE run_id='one'"); got != 1 {
		t.Fatalf("partial entries: %d", got)
	}
	if got := ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='one'"); got != 2 {
		t.Fatalf("failed writes consumed sequence: %d", got)
	}
}

func TestReferenceFailureRollsBackEntry(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	startLedger(t, s, "run-1", "")
	addLedger(t, s, "run-1", "e1", KindEvidence, Evidence{Fact: "fact", Source: "query"})
	h := hypothesis("e1")
	h.Contradicting = []string{"missing"}
	raw, _ := json.Marshal(h)
	if _, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: "h1", Kind: KindHypothesis, Payload: raw}); !errors.Is(err, ErrInvalidReference) {
		t.Fatal(err)
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs") != 0 || ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 1 {
		t.Fatal("partial reference or entry committed")
	}
	entry := addLedger(t, s, "run-1", "h1", KindHypothesis, hypothesis("e1"))
	if entry.Seq != 2 || ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs") != 1 {
		t.Fatal("rollback did not restore sequence")
	}
}

func TestIdempotentRetryAndConflict(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	first := startLedger(t, s, "run-1", "")
	retry := startLedger(t, s, "run-1", "")
	if !reflect.DeepEqual(first, retry) {
		t.Fatal("run retry changed original")
	}
	if _, err := s.Start(t.Context(), StartRequest{ID: "run-1", Problem: "other"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed start accepted: %v", err)
	}
	entry := addLedger(t, s, "run-1", "e1", KindEvidence, Evidence{Fact: "same", Source: "query"})
	again, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: "e1", Kind: KindEvidence, Payload: json.RawMessage(`{ "source": "query", "fact": "same" }`)})
	if err != nil || !reflect.DeepEqual(entry, again) {
		t.Fatalf("retry changed original: %+v %v", again, err)
	}
	for _, req := range []AddRequest{
		{RunID: "run-1", ID: "e1", Kind: KindEvidence, Payload: json.RawMessage(`{"fact":"changed","source":"query"}`)},
		{RunID: "run-1", ID: "e1", Kind: KindMetadata, Payload: json.RawMessage(`{"phase":1}`)},
	} {
		if _, err := s.Add(t.Context(), req); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict accepted: %v", err)
		}
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 1 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 2 {
		t.Fatal("replay or conflict changed database")
	}
	addLedger(t, s, "run-1", "rename", KindMetadata, map[string]string{"problem": "refined problem"})
	if got := startLedger(t, s, "run-1", ""); got.Problem != "refined problem" {
		t.Fatal("start replay undid metadata")
	}
	generated, err := s.Start(t.Context(), StartRequest{Problem: "generated"})
	if err != nil || len(generated.ID) != 36 || !strings.HasPrefix(generated.ID, "run-") {
		t.Fatalf("generated ID: %q %v", generated.ID, err)
	}
}

func TestConcurrentWritersPreserveMetadata(t *testing.T) {
	root := t.TempDir()
	s1, _ := openLedger(t, root)
	s2, _ := openLedger(t, root)
	startLedger(t, s1, "run-1", "")
	start := make(chan struct{})
	errs := make(chan error, 20)
	var workers sync.WaitGroup
	for i := range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			s := s1
			payload := map[string]any{"impact": fmt.Sprint(i)}
			if i%2 == 0 {
				s = s2
				payload = map[string]any{"environment": map[string]string{"context": fmt.Sprint(i)}}
			}
			raw, _ := json.Marshal(payload)
			_, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: fmt.Sprintf("m%d", i), Kind: KindMetadata, Payload: raw})
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err := s1.Load(t.Context(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var impact, kubeContext string
	for i, e := range run.Entries {
		if e.Seq != int64(i+1) {
			t.Fatal("noncontiguous committed sequence")
		}
		m := decoded[MetadataUpdate](t, e)
		if m.Impact != nil {
			impact = *m.Impact
		}
		if m.Environment != nil {
			kubeContext = m.Environment.Context
		}
	}
	if len(run.Entries) != 20 || run.State.Impact != impact || run.State.Environment.Context != kubeContext || impact == "" || kubeContext == "" {
		t.Fatal("concurrent patches lost data")
	}
}

func TestSessionBindingsIsolateConcurrentRuns(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	a, b := "../session", strings.Repeat("s", 256)
	first := startLedger(t, s, "one", a)
	startLedger(t, s, "two", b)
	startLedger(t, s, "latest", "")
	for session, id := range map[string]string{a: "one", b: "two"} {
		run, err := s.LookupSession(t.Context(), session)
		if err != nil || run.ID != id {
			t.Fatalf("session selected wrong run: %v", err)
		}
	}
	old := addLedger(t, s, "one", "bind", KindMetadata, map[string]string{"session_id": a})
	addLedger(t, s, "two", "rebind", KindMetadata, map[string]string{"session_id": a})
	if _, err := s.Add(t.Context(), AddRequest{RunID: "one", ID: old.ID, Kind: old.Kind, Payload: old.Payload}); err != nil {
		t.Fatal(err)
	}
	startLedger(t, s, "one", a)
	bound, err := s.LookupSession(t.Context(), a)
	if err != nil || bound.ID != "two" {
		t.Fatal("idempotent replay restored old binding")
	}
	if len(first.Entries) != 1 || *decoded[MetadataUpdate](t, first.Entries[0]).SessionID != a {
		t.Fatal("initial session metadata not atomic")
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM run_sessions") != 2 {
		t.Fatal("session identity mutated")
	}
	if _, err := s.LookupSession(t.Context(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	other, _ := openLedger(t, t.TempDir())
	if _, err := other.LookupSession(t.Context(), a); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := other.Load(t.Context(), "one"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	latest, err := s.Latest(t.Context())
	if err != nil || latest.ID != "latest" {
		t.Fatal("latest follows activity rather than creation")
	}
}

func TestEveryTypedReferenceAndPragmas(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	startLedger(t, s, "run-1", "")
	for _, e := range ledgerFixtures() {
		if _, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: e.ID, Kind: e.Kind, Payload: e.Payload}); err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}
	}
	run, err := s.Load(t.Context(), "run-1")
	if err != nil || len(run.Entries) != 11 || run.ThroughSeq != 11 {
		t.Fatalf("full load: %v", err)
	}
	if got := ledgerInt(t, db.DB(), "PRAGMA foreign_keys"); got != 1 {
		t.Fatalf("foreign keys=%d", got)
	}
	if got := ledgerInt(t, db.DB(), "PRAGMA synchronous"); got != 2 {
		t.Fatalf("synchronous=%d", got)
	}
	if got := ledgerInt(t, db.DB(), "PRAGMA busy_timeout"); got != 5000 {
		t.Fatalf("busy_timeout=%d", got)
	}
	if got := ledgerInt(t, db.DB(), "PRAGMA user_version"); got != 4 {
		t.Fatalf("schema=%d", got)
	}
	if got := ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs"); got != 11 {
		t.Fatalf("typed refs=%d", got)
	}
	if _, err := db.DB().ExecContext(t.Context(), "INSERT INTO ledger_refs VALUES ('run-1','e1','missing','evidence')"); err == nil {
		t.Fatal("foreign key not enforced")
	}
	// Replacement connections must receive the same write guarantees.
	db.DB().SetMaxIdleConns(0)
	addLedger(t, s, "run-1", "e3", KindEvidence, Evidence{Fact: "replacement", Source: "query"})
}

func TestMissingRunAndCorruptPayloadAreExplicitErrors(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	if _, err := s.Latest(t.Context()); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no runs recorded") {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Add(t.Context(), AddRequest{RunID: "missing", ID: "e1", Kind: KindEvidence, Payload: json.RawMessage(`{"fact":"fact","source":"query"}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	startLedger(t, s, "one", "")
	addLedger(t, s, "one", "e1", KindEvidence, Evidence{Fact: "fact", Source: "query"})
	if _, err := db.DB().ExecContext(t.Context(), `UPDATE ledger_entries SET payload_json='{"unexpected":"password=never-print"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context(), "one"); !errors.Is(err, ErrCorrupt) || strings.Contains(err.Error(), "never-print") {
		t.Fatalf("corruption hidden or leaked: %v", err)
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 1 {
		t.Fatal("corrupt database reset")
	}
}

func TestLedgerLockDeadlineAndRollback(t *testing.T) {
	root := t.TempDir()
	s, db := openLedger(t, root)
	_, locker := openLedger(t, root)
	startLedger(t, s, "one", "")
	tx, err := locker.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE runs SET next_seq=next_seq WHERE id='one'"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err = s.Add(ctx, AddRequest{RunID: "one", ID: "e1", Kind: KindEvidence, Payload: json.RawMessage(`{"fact":"fact","source":"query"}`)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 0 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 1 {
		t.Fatal("canceled write persisted")
	}
	addLedger(t, s, "one", "e1", KindEvidence, Evidence{Fact: "fact", Source: "query"})
}

func TestLoadRunUsesCallerSnapshot(t *testing.T) {
	root := t.TempDir()
	s, db := openLedger(t, root)
	other, _ := openLedger(t, root)
	startLedger(t, s, "one", "")
	tx, err := db.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var seq int
	if err := tx.QueryRowContext(t.Context(), "SELECT next_seq FROM runs WHERE id='one'").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	addLedger(t, other, "one", "e1", KindEvidence, Evidence{Fact: "later", Source: "query"})
	run, err := LoadRun(t.Context(), tx, "one")
	if err != nil || len(run.Entries) != 0 || run.ThroughSeq != 0 {
		t.Fatalf("snapshot torn: %v", err)
	}
}

func TestKilledWriterResumesCommittedEntries(t *testing.T) {
	root := t.TempDir()
	s, _ := openLedger(t, root)
	startLedger(t, s, "one", "")
	worker, lines := ledgerWorker(t, root, "write")
	for i := 1; i <= 20; i++ {
		if got := <-lines; got != fmt.Sprintf("ACK e%d", i) {
			t.Fatalf("ACK %d: %q", i, got)
		}
	}
	killLedgerWorker(t, worker)
	verifyKilledRun(t, root)
	worker, lines = ledgerWorker(t, root, "pending")
	if got := <-lines; got != "PENDING" {
		t.Fatalf("pending marker: %q", got)
	}
	killLedgerWorker(t, worker)
	verifyKilledRun(t, root)
}

func ledgerWorker(t *testing.T, root, mode string) (*exec.Cmd, <-chan string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLedgerProcess$")
	cmd.Env = append(os.Environ(), "SRE_LEDGER_PROCESS="+mode, "SRE_LEDGER_ROOT="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
		<-finished
	})
	go func() {
		defer close(finished)
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	return cmd, lines
}

func killLedgerWorker(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("worker exited without SIGKILL")
	}
}

func verifyKilledRun(t *testing.T, root string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLedgerProcess$")
	cmd.Env = append(os.Environ(), "SRE_LEDGER_PROCESS=load", "SRE_LEDGER_ROOT="+root)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("new process load: %v %s", err, output)
	}
	var run Run
	if err := json.Unmarshal(output, &run); err != nil {
		t.Fatalf("new process JSON: %v %s", err, output)
	}
	if len(run.Entries) != 20 || run.ThroughSeq != 20 {
		t.Fatalf("committed history lost: %d", len(run.Entries))
	}
	for i, entry := range run.Entries {
		if entry.ID != fmt.Sprintf("e%d", i+1) || decoded[Evidence](t, entry).Fact != fmt.Sprintf("fact%d", i+1) {
			t.Fatal("ACKed payload changed")
		}
	}
}

func TestLedgerProcess(t *testing.T) {
	mode := os.Getenv("SRE_LEDGER_PROCESS")
	if mode == "" {
		return
	}
	s, db := openLedger(t, os.Getenv("SRE_LEDGER_ROOT"))
	if mode == "load" {
		run, err := s.Load(t.Context(), "one")
		if err != nil {
			t.Fatal(err)
		}
		if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs WHERE entry_id='h21'") != 0 {
			t.Fatal("uncommitted reference retained")
		}
		if err := json.NewEncoder(os.Stdout).Encode(run); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	if mode == "write" {
		for i := 1; i <= 20; i++ {
			entry := addLedger(t, s, "one", fmt.Sprintf("e%d", i), KindEvidence, Evidence{Fact: fmt.Sprintf("fact%d", i), Source: "query"})
			fmt.Println("ACK", entry.ID)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if mode == "unacknowledged" {
		addLedger(t, s, "one", "e1", KindEvidence, Evidence{Fact: "committed", Source: "query"})
		fmt.Println("COMMITTED")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	s.beforeCommit = func(*sql.Conn) error {
		fmt.Println("PENDING")
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return err
	}
	addLedger(t, s, "one", "h21", KindHypothesis, hypothesis("e1"))
	t.Fatal("pending transaction should have been killed")
}

func TestSecretNeverStored(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	secret := "Authorization: Bearer auth-value https://user:credential@example.test password=private-value\n-----BEGIN PRIVATE KEY-----\nfixture-material\n-----END PRIVATE KEY-----"
	run, err := s.Start(t.Context(), StartRequest{ID: "run-1", Problem: secret})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(run.Problem, "private-value") {
		t.Fatal("start problem not redacted")
	}
	for _, e := range ledgerFixtures() {
		value, err := DecodePayload(e.Kind, e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		v := reflect.New(reflect.TypeOf(value))
		v.Elem().Set(reflect.ValueOf(value))
		poisonHumanText(v.Elem(), secret)
		raw, err := json.Marshal(v.Elem().Interface())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: e.ID, Kind: e.Kind, Payload: raw}); err != nil {
			t.Fatalf("%s: %v", e.Kind, err)
		}
	}
	root := filepath.Dir(filepath.Dir(databasePath(t, db.DB())))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, db = openLedger(t, root)
	loaded, err := s.Load(t.Context(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(loaded)
	rows, err := db.DB().QueryContext(t.Context(), "SELECT problem FROM runs UNION ALL SELECT payload_json FROM ledger_entries")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	all := string(encoded)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		all += raw
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"auth-value", "user:credential", "private-value", "fixture-material"} {
		if strings.Contains(all, needle) {
			t.Fatalf("secret persisted: %s", needle)
		}
	}
	if !strings.Contains(all, "[REDACTED]") {
		t.Fatal("redaction was not exercised")
	}
}

func databasePath(t *testing.T, db *sql.DB) string {
	t.Helper()
	var seq int
	var name, path string
	if err := db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}

func poisonHumanText(v reflect.Value, secret string) {
	if v.Type() == reflect.TypeFor[time.Time]() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			poisonHumanText(v.Elem(), secret)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			name := v.Type().Field(i).Name
			if name == "SessionID" || name == "ID" || strings.HasSuffix(name, "ID") || strings.HasSuffix(name, "IDs") || oneOf(name, "Supporting", "Contradicting", "Supersedes", "Acknowledges", "Confidence", "Status", "Decision", "Effect", "Verdict", "MutationCheck", "Change") {
				continue
			}
			if value, ok := v.Interface().(SafetyChange); ok && name == "Object" && value.Change == "head" {
				continue
			}
			poisonHumanText(v.Field(i), secret)
		}
	case reflect.String:
		v.SetString(secret)
	case reflect.Slice:
		for i := range v.Len() {
			poisonHumanText(v.Index(i), secret)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			v.SetMapIndex(key, reflect.ValueOf(secret))
		}
	}
}

func TestUnacknowledgedCommitCanBeRetried(t *testing.T) {
	root := t.TempDir()
	s, _ := openLedger(t, root)
	startLedger(t, s, "one", "")
	cmd, lines := ledgerWorker(t, root, "unacknowledged")
	if got := <-lines; got != "COMMITTED" {
		t.Fatalf("commit control: %q", got)
	}
	killLedgerWorker(t, cmd)
	reopened, db := openLedger(t, root)
	entry := addLedger(t, reopened, "one", "e1", KindEvidence, Evidence{Fact: "committed", Source: "query"})
	if entry.Seq != 1 || ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 1 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 2 {
		t.Fatal("uncertain completion retry duplicated the entry")
	}
}

func TestCommitFailureAndPostInsertCancellationNeverAcknowledge(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	startLedger(t, s, "one", "session")
	s.beforeCommit = func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(t.Context(), "PRAGMA defer_foreign_keys=ON"); err != nil {
			return err
		}
		_, err := conn.ExecContext(t.Context(), "INSERT INTO run_sessions(session_id,run_id) VALUES (?,?)", "password=not-in-errors", "missing")
		return err
	}
	entry, err := s.Add(t.Context(), AddRequest{RunID: "one", ID: "e1", Kind: KindEvidence, Payload: json.RawMessage(`{"fact":"fact","source":"query"}`)})
	if err == nil || entry.ID != "" || strings.Contains(err.Error(), "not-in-errors") {
		t.Fatalf("failed COMMIT acknowledged or leaked: %+v %v", entry, err)
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 1 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='one'") != 2 || ledgerInt(t, db.DB(), "SELECT count(*) FROM run_sessions") != 1 {
		t.Fatal("failed COMMIT left partial writes")
	}
	if run, err := s.Start(t.Context(), StartRequest{ID: "two", Problem: "other", SessionID: "new-session"}); err == nil || run.ID != "" {
		t.Fatal("failed start COMMIT acknowledged")
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM runs") != 1 || ledgerInt(t, db.DB(), "SELECT count(*) FROM run_sessions") != 1 {
		t.Fatal("failed start left run or binding")
	}
	ctx, cancel := context.WithCancel(t.Context())
	s.beforeCommit = func(*sql.Conn) error { cancel(); return nil }
	_, err = s.Add(ctx, AddRequest{RunID: "one", ID: "rebind", Kind: KindMetadata, Payload: json.RawMessage(`{"session_id":"new-session"}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled after insert: %v", err)
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 1 || ledgerInt(t, db.DB(), "SELECT count(*) FROM run_sessions") != 1 {
		t.Fatal("canceled binding persisted")
	}
	s.beforeCommit = nil
	addLedger(t, s, "one", "e1", KindEvidence, Evidence{Fact: "recovered", Source: "query"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context(), "one"); err == nil {
		t.Fatal("closed store read succeeded")
	}
	if _, err := s.Start(t.Context(), StartRequest{ID: "closed", Problem: "problem"}); err == nil {
		t.Fatal("closed store write succeeded")
	}
}

func TestCanonicalRedactionKeepsIdentitiesAndSensitiveCoordinates(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	const session = "token=opaque-session/../identity"
	startLedger(t, s, "secret-run", session)
	entry := addLedger(t, s, "secret-run", "token-entry", KindMetadata, map[string]any{"environment": map[string]any{"cloud_coordinates": map[string]string{"password": "plain-coordinate-secret", "region": "safe"}}, "session_id": session})
	metadata := decoded[MetadataUpdate](t, entry)
	if *metadata.SessionID != session || metadata.Environment.CloudCoordinates["password"] != "[REDACTED]" || metadata.Environment.CloudCoordinates["region"] != "safe" {
		t.Fatal("identity changed or coordinate leaked")
	}
	bound, err := s.LookupSession(t.Context(), session)
	if err != nil || bound.ID != "secret-run" {
		t.Fatal("opaque session was rewritten")
	}
	original := addLedger(t, s, "secret-run", "redacted-retry", KindEvidence, Evidence{Fact: "password=one-secret", Source: "query"})
	replayed := addLedger(t, s, "secret-run", "redacted-retry", KindEvidence, Evidence{Fact: "password=other-secret", Source: "query"})
	if !reflect.DeepEqual(original, replayed) {
		t.Fatal("equal canonical redacted payload was not idempotent")
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries WHERE id='redacted-retry'") != 1 {
		t.Fatal("duplicate redacted retry")
	}
	h := hypothesis("missing")
	h.Statement = "password=failed-secret"
	raw, _ := json.Marshal(h)
	if _, err := s.Add(t.Context(), AddRequest{RunID: "secret-run", ID: "failed", Kind: KindHypothesis, Payload: raw}); err == nil || strings.Contains(err.Error(), "failed-secret") {
		t.Fatalf("failed write leaked: %v", err)
	}
}

func TestAllReferenceKindsRejectWrongTargetsAndSelfAcknowledgment(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	startLedger(t, s, "run-1", "")
	for _, e := range ledgerFixtures() {
		if _, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: e.ID, Kind: e.Kind, Payload: e.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		index         int
		field, target string
	}{
		{3, "supporting", "m1"}, {3, "contradicting", "m1"}, {3, "supersedes", "e1"},
		{5, "hypothesis_ids", "e1"}, {6, "option_id", "h1"}, {7, "option_id", "p1"},
		{7, "approval_id", "o1"}, {7, "evidence_ids", "h1"}, {8, "expected_entry_id", "o1"},
		{8, "action_id", "e1"}, {8, "evidence_ids", "h1"}, {10, "acknowledges", "m1"},
	} {
		e := ledgerFixtures()[tc.index]
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if _, array := payload[tc.field].([]any); array {
			payload[tc.field] = []string{tc.target}
		} else {
			payload[tc.field] = tc.target
		}
		raw, _ := json.Marshal(payload)
		if _, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: "invalid", Kind: e.Kind, Payload: raw}); !errors.Is(err, ErrInvalidReference) {
			t.Fatalf("%s.%s accepted: %v", e.Kind, tc.field, err)
		}
	}
	self := SafetyViolation{Wave: 1, Investigators: []string{}, Changes: []SafetyChange{}, Acknowledges: "self"}
	raw, _ := json.Marshal(self)
	if _, err := s.Add(t.Context(), AddRequest{RunID: "run-1", ID: "self", Kind: KindSafetyViolation, Payload: raw}); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("self acknowledgment: %v", err)
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 11 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 12 {
		t.Fatal("invalid reference changed run")
	}
}

func TestLedgerMigrationUpgradesVersionOne(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".sre-agent"), 0700); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(root, ".sre-agent", "sre.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec("CREATE TABLE preserved(value TEXT); INSERT INTO preserved VALUES ('old'); PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, db := openLedger(t, root)
	if ledgerInt(t, db.DB(), "PRAGMA user_version") != 4 || ledgerInt(t, db.DB(), "SELECT count(*) FROM preserved WHERE value='old'") != 1 {
		t.Fatal("version1 migration lost previous data")
	}
	for _, name := range []string{"runs", "ledger_entries", "ledger_refs", "run_sessions"} {
		if ledgerInt(t, db.DB(), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name) != 1 {
			t.Fatalf("missing table %s", name)
		}
	}
	startLedger(t, s, "one", "")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ = openLedger(t, root)
	if run, err := s.Load(t.Context(), "one"); err != nil || run.ID != "one" {
		t.Fatal("migration rerun lost run")
	}
}

func TestLoadRejectsCorruptTypedReferences(t *testing.T) {
	for _, target := range []string{"missing", "h1", "later", "metadata"} {
		t.Run(target, func(t *testing.T) {
			s, db := openLedger(t, t.TempDir())
			startLedger(t, s, "one", "")
			addLedger(t, s, "one", "e1", KindEvidence, Evidence{Fact: "fact", Source: "query"})
			addLedger(t, s, "one", "metadata", KindMetadata, map[string]int{"phase": 1})
			addLedger(t, s, "one", "h1", KindHypothesis, hypothesis("e1"))
			addLedger(t, s, "one", "later", KindEvidence, Evidence{Fact: "later", Source: "query"})
			raw, _ := json.Marshal(hypothesis(target))
			if _, err := db.DB().ExecContext(t.Context(), "UPDATE ledger_entries SET payload_json=? WHERE id='h1'", string(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(t.Context(), "one"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt %s reference returned: %v", target, err)
			}
			if _, err := s.Add(t.Context(), AddRequest{RunID: "one", ID: "h1", Kind: KindHypothesis, Payload: raw}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt replay acknowledged: %v", err)
			}
			if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 4 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 5 {
				t.Fatal("corrupt replay changed state")
			}
		})
	}
}
