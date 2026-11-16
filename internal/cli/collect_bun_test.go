package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/coderuntime"
	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
)

func requireCLIBun(t *testing.T) {
	t.Helper()
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("set SRE_TEST_BUN_RUNTIME=1 to opt in to real Bun collection")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := coderuntime.New().Probe(ctx); err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
}

func publicCollectionCLI(t *testing.T, root, input, id string, extra ...string) (int, collectionOutput) {
	t.Helper()
	var out, stderr bytes.Buffer
	args := append([]string{"collect", "--repo", root, "--plan", "-", "--run", "run", "--id", id, "--timeout", "30s"}, extra...)
	code := RunWithInput(args, strings.NewReader(input), &out, &stderr)
	var envelope struct {
		OK   bool             `json:"ok"`
		Data collectionOutput `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.OK != (code == 0) {
		t.Fatalf("invalid collection output: code=%d error=%v stdout=%s stderr=%s", code, err, &out, &stderr)
	}
	return code, envelope.Data
}

func hideBunFromCLI(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	if err := os.Symlink(git, filepath.Join(path, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path)
}

func TestCollectBunCLI(t *testing.T) {
	requireCLIBun(t)
	root, measure, _ := collectionFixture(t)
	const plan = `{"calls":[{"key":"metric","source_id":"mcp-metrics","tool":"query","arguments":{"query":"queue_depth"}}],"required":["metric"],"operation":"filter"}`
	const program = `const response = await collection.call("metric");
if (!response.ok) throw new Error("helper failed");
return [{key:"metric",source_id:collection.source_id,data:{queue:Number(response.result.data.structuredContent.data.result[0].value[1])}}];`
	path := filepath.Join(root, "program.js")
	if err := os.WriteFile(path, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := publicCollectionCLI(t, root, plan, "code", "--mode", "code", "--program", path, "--approve-code")
	hash := sha256.Sum256([]byte(program))
	if code != 0 || !out.Committed || out.Result == nil || out.Receipt == nil || out.Result.SelectedMode != collect.ModeCode || out.Result.CodeSHA256 != hex.EncodeToString(hash[:]) || !out.Receipt.Success {
		t.Fatalf("public CLI did not execute Bun: code=%d result=%+v receipt=%+v", code, out.Result, out.Receipt)
	}
	if len(out.Result.Evidence) != 1 || out.Result.Evidence[0].Origin != "program_derived" || string(out.Result.Evidence[0].Data) != `{"queue":12}` {
		t.Fatalf("code evidence missing: %+v", out.Result.Evidence)
	}
	_, calls := measure.snapshot("mcp-metrics", "queue_depth")
	if calls != 1 {
		t.Fatalf("helper requests=%d, want 1", calls)
	}
	db, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range run.Entries {
		for _, forbidden := range []string{"collection.call", "fixture-read-credential", "structuredContent", `\"queue\"`, "audit", "arguments", "input_tokens"} {
			if bytes.Contains(entry.Payload, []byte(forbidden)) {
				t.Fatalf("raw data in ledger: %q", forbidden)
			}
		}
	}
	if err := os.Remove(filepath.Join(root, sources.RegistryFilename)); err != nil {
		t.Fatal(err)
	}
	t.Run("replay without sources or runtime", func(t *testing.T) {
		hideBunFromCLI(t)
		t.Setenv("SRE_FIXTURE_READ_TOKEN", "")
		before, _ := measure.snapshot("mcp-metrics", "queue_depth")
		exit, replay := publicCollectionCLI(t, root, plan, "code", "--mode", "code", "--program", path, "--approve-code")
		after, _ := measure.snapshot("mcp-metrics", "queue_depth")
		if exit != 0 || !replay.Replayed || !replay.Committed || replay.Result != nil || replay.Receipt == nil || replay.Receipt.CodeSHA256 != out.Receipt.CodeSHA256 || len(before) != len(after) {
			t.Fatalf("receipt replay repeated collection or lost identity: exit=%d %+v", exit, replay)
		}
	})

	const twoCalls = `{"calls":[{"key":"metric","source_id":"mcp-metrics","tool":"query","arguments":{"query":"queue_depth"}},{"key":"other","source_id":"mcp-metrics","tool":"query","arguments":{"query":"queue_depth"}}],"required":["metric","other"],"operation":"filter"}`
	const allKeys = `const values=[];for(const key of collection.keys){const response=await collection.call(key.key);if(!response.ok)throw new Error();values.push({key:key.key,source_id:key.source_id,data:response.result.data});}return values;`
	for _, tc := range []struct {
		name, mode, input, program, reason string
		consent, noGrant, noRuntime        bool
		selected                           collect.Mode
		exit, calls                        int
	}{
		{"manual ordinary without Bun", "ordinary", plan, allKeys, "manual ordinary collection", false, false, true, collect.ModeOrdinary, 0, 1},
		{"short auto without Bun", "auto", plan, allKeys, "short or independent collection", true, false, true, collect.ModeOrdinary, 0, 1},
		{"independent auto without Bun", "auto", strings.Replace(twoCalls, "filter", "independent", 1), allKeys, "short or independent collection", true, false, true, collect.ModeOrdinary, 0, 2},
		{"eligible auto", "auto", twoCalls, allKeys, "eligible code collection", true, false, false, collect.ModeCode, 0, 2},
		{"missing consent", "code", twoCalls, allKeys, "code consent missing", false, false, false, collect.ModeNone, 1, 0},
		{"missing grant", "code", twoCalls, allKeys, "code source grant missing", true, true, false, collect.ModeNone, 1, 0},
		{"unavailable runtime", "code", twoCalls, allKeys, "code runtime unavailable", true, false, true, collect.ModeNone, 1, 0},
		{"auto missing consent", "auto", twoCalls, allKeys, "code consent missing", false, false, false, collect.ModeOrdinary, 0, 2},
		{"auto missing grant", "auto", twoCalls, allKeys, "code source grant missing", true, true, false, collect.ModeOrdinary, 0, 2},
		{"auto unavailable runtime", "auto", twoCalls, allKeys, "code runtime unavailable", true, false, true, collect.ModeOrdinary, 0, 2},
		{"execution failure no fallback", "auto", twoCalls, `await collection.call("metric");throw new Error("program failed");`, "eligible code collection", true, false, false, collect.ModeCode, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, measure, _ := collectionFixture(t)
			if tc.noGrant {
				registry, err := sources.Load(t.Context(), root)
				if err != nil {
					t.Fatal(err)
				}
				source := registry.Sources[0].Source
				for _, s := range registry.Sources {
					if s.ID == "mcp-metrics" {
						source = s.Source
					}
				}
				source.CodeNetwork = nil
				if _, err := sources.Approve(t.Context(), root, sources.ApprovalRequest{Approved: true, Sources: []sources.Source{source}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.noRuntime {
				hideBunFromCLI(t)
			}
			path := filepath.Join(root, "program.js")
			if err := os.WriteFile(path, []byte(tc.program), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--mode", tc.mode, "--program", path}
			if tc.consent {
				args = append(args, "--approve-code")
			}
			exit, got := publicCollectionCLI(t, root, tc.input, "selection", args...)
			_, calls := measure.snapshot("mcp-metrics", "queue_depth")
			if exit != tc.exit || !got.Committed || got.Result == nil || got.Result.SelectedMode != tc.selected || got.Result.Reason != tc.reason || calls != tc.calls {
				t.Fatalf("selection or execution changed: exit=%d calls=%d result=%+v", exit, calls, got.Result)
			}
			if tc.name == "execution failure no fallback" {
				found := false
				for _, gap := range got.Result.Gaps {
					found = found || gap.Reason == "code execution failed"
				}
				if !found || got.Receipt.Success {
					t.Fatalf("lost execution failure: %+v", got.Result)
				}
			}
		})
	}
}
