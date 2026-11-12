package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTemplateSecretsNeverEscape(t *testing.T) {
	raw := json.RawMessage(`{"metadata":{"annotations":{"anything":"ANNOTATION_CANARY"},"labels":{"pod-template-hash":"generated"}},"spec":{"containers":[{"name":"api","image":"app:v1","command":["COMMAND_CANARY"],"args":["ARGS_CANARY"],"env":[{"name":"PLAIN","value":"ENV_CANARY"},{"name":"REF","valueFrom":{"secretKeyRef":{"name":"app-credentials","key":"login"}}}],"readinessProbe":{"httpGet":{"path":"/ready","port":8080,"httpHeaders":[{"name":"X-Key","value":"HEADER_CANARY"}]},"failureThreshold":3},"lifecycle":{"preStop":{"exec":{"command":["HOOK_CANARY"]}}},"unknownField":{"arbitrary":"UNKNOWN_CANARY"}}]}}`)
	safe, excluded, err := ProjectTemplate(raw)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(struct {
		Safe     any
		Excluded any
		Diff     any
	}{safe, excluded, DiffTemplates(nil, safe)})
	for _, secret := range []string{"ANNOTATION_CANARY", "COMMAND_CANARY", "ARGS_CANARY", "ENV_CANARY", "HEADER_CANARY", "HOOK_CANARY", "UNKNOWN_CANARY", "generated"} {
		if strings.Contains(string(output), secret) {
			t.Fatalf("excluded value escaped: %s", secret)
		}
	}
	for _, want := range []string{"app:v1", "app-credentials", "login", "/ready", "failureThreshold"} {
		if !strings.Contains(string(output), want) {
			t.Errorf("missing safe field %s: %s", want, output)
		}
	}
	if len(excluded) < 6 {
		t.Fatalf("missing coverage: %v", excluded)
	}
	changed := strings.ReplaceAll(string(raw), "ENV_CANARY", "OTHER_SECRET")
	changed = strings.ReplaceAll(changed, "generated", "newhash")
	other, _, err := ProjectTemplate(json.RawMessage(changed))
	if err != nil {
		t.Fatal(err)
	}
	if changes := DiffTemplates(safe, other); len(changes) != 0 {
		t.Fatalf("hidden changes must not be inferred: %+v", changes)
	}
}

func TestTemplateSafeFieldsAndStableDiff(t *testing.T) {
	before := json.RawMessage(`{"spec":{"serviceAccountName":"worker","terminationGracePeriodSeconds":30,"nodeSelector":{"pool":"batch"},"securityContext":{"runAsNonRoot":true},"containers":[{"name":"api","image":"app:v1","resources":{"requests":{"cpu":"100m"}},"ports":[{"name":"http","containerPort":8080}],"livenessProbe":{"tcpSocket":{"port":8080},"periodSeconds":10},"volumeMounts":[{"name":"config","mountPath":"/etc/app"}]}],"initContainers":[{"name":"setup","image":"setup:v1"}],"volumes":[{"name":"config","configMap":{"name":"config-v1","items":[{"key":"config","path":"app.conf"}]}}]}}`)
	a, gaps, err := ProjectTemplate(before)
	if err != nil || len(gaps) > 0 {
		t.Fatalf("projection: %v %v", err, gaps)
	}
	after := strings.ReplaceAll(string(before), "app:v1", "app:v2")
	after = strings.ReplaceAll(after, "100m", "200m")
	after = strings.ReplaceAll(after, "config-v1", "config-v2")
	after = strings.ReplaceAll(after, `"periodSeconds":10`, `"periodSeconds":20`)
	b, _, err := ProjectTemplate(json.RawMessage(after))
	if err != nil {
		t.Fatal(err)
	}
	changes := DiffTemplates(a, b)
	got := map[string][2]any{}
	for _, c := range changes {
		got[c.Path] = [2]any{c.Before, c.After}
	}
	want := map[string][2]any{"spec.containers[api].image": {"app:v1", "app:v2"}, "spec.containers[api].resources.requests.cpu": {"100m", "200m"}, "spec.containers[api].livenessProbe.periodSeconds": {float64(10), float64(20)}, "spec.volumes[config].configMap.name": {"config-v1", "config-v2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deltas=%#v", got)
	}
}

func TestTemplateInvalidInput(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"spec":null}`, `{"spec":{"containers":[]}}`, `{"spec":{"containers":"wrong"}}`, `{"spec":{"containers":[{"name":"a","image":{"hidden":"SECRET"}}]}}`, `{"spec":{"containers":[{"name":"a"},{"name":"a"}]}}`} {
		_, _, err := ProjectTemplate(json.RawMessage(raw))
		if err == nil {
			t.Errorf("accepted invalid template %s", raw)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("raw input in diagnostic")
		}
	}
}

func TestTemplateSchedulingSecurityAndReferences(t *testing.T) {
	raw := json.RawMessage(`{"spec":{"containers":[{"name":"main","image":"app:v1","securityContext":{"capabilities":{"drop":["ALL"]},"runAsNonRoot":true},"lifecycle":{"preStop":{"sleep":{"seconds":5}}},"envFrom":[{"configMapRef":{"name":"settings"}},{"secretRef":{"name":"credentials"}}]}],"initContainers":[{"name":"init","image":"init:v1"}],"serviceAccountName":"worker","terminationGracePeriodSeconds":30,"securityContext":{"seccompProfile":{"type":"RuntimeDefault"},"supplementalGroups":[1000]},"nodeSelector":{"pool":"batch"},"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"pool","operator":"In","values":["batch"]}]}]}}},"tolerations":[{"key":"batch","operator":"Exists","effect":"NoSchedule"}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-pvc","readOnly":true}},{"name":"projected","projected":{"sources":[{"secret":{"name":"credentials","items":[{"key":"login","path":"login"}]}},{"serviceAccountToken":{"path":"token","expirationSeconds":3600}}]}}]}}`)
	got, gaps, err := ProjectTemplate(raw)
	if err != nil || len(gaps) != 0 {
		t.Fatalf("projection err=%v gaps=%v", err, gaps)
	}
	for path, want := range map[string]any{
		"spec.initContainers[init].image":                            "init:v1",
		"spec.containers[main].securityContext.capabilities.drop[0]": "ALL",
		"spec.containers[main].lifecycle.preStop.sleep.seconds":      float64(5),
		"spec.containers[main].envFrom[0].configMapRef.name":         "settings",
		"spec.containers[main].envFrom[1].secretRef.name":            "credentials",
		"spec.serviceAccountName":                                    "worker",
		"spec.terminationGracePeriodSeconds":                         float64(30),
		"spec.securityContext.seccompProfile.type":                   "RuntimeDefault",
		"spec.securityContext.supplementalGroups[0]":                 float64(1000),
		"spec.nodeSelector.pool":                                     "batch",
		"spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]": "batch",
		"spec.volumes[data].persistentVolumeClaim.claimName":                                                                           "data-pvc",
		"spec.volumes[projected].projected.sources[0].secret.items[0].key":                                                             "login",
		"spec.volumes[projected].projected.sources[1].serviceAccountToken.expirationSeconds":                                           float64(3600),
	} {
		if !reflect.DeepEqual(got[path], want) {
			t.Errorf("%s = %v, want %v", path, got[path], want)
		}
	}
}

func TestTemplateReorderAndUnknownCoverage(t *testing.T) {
	a, _, err := ProjectTemplate(json.RawMessage(`{"spec":{"containers":[{"name":"a","image":"a:v1"},{"name":"b","image":"b:v1"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, gaps, err := ProjectTemplate(json.RawMessage(`{"metadata":{"annotations":{"unknown":"SECRET_VALUE"}},"spec":{"SECRET_FIELD":"SECRET_VALUE","containers":[{"name":"b","image":"b:v1"},{"name":"a","image":"a:v1"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(DiffTemplates(a, b)) != 0 || len(gaps) != 2 {
		t.Fatalf("changes=%v gaps=%v", DiffTemplates(a, b), gaps)
	}
	output, _ := json.Marshal(gaps)
	if strings.Contains(string(output), "SECRET") {
		t.Fatal("unknown key escaped coverage diagnostic")
	}
}

func TestTemplateProbeFieldsAreNotLifecycleFields(t *testing.T) {
	safe, gaps, err := ProjectTemplate(json.RawMessage(`{"spec":{"containers":[{"name":"api","image":"app:v1","livenessProbe":{"httpGet":{"path":"/live","port":8080},"sleep":{"seconds":77}}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, retained := safe["spec.containers[api].livenessProbe.sleep.seconds"]; retained || len(gaps) != 1 {
		t.Fatalf("undocumented probe field retained: %v gaps=%v", safe, gaps)
	}
}

func TestTemplateEnvironmentIdentitiesAndDuplicates(t *testing.T) {
	raw := json.RawMessage(`{"spec":{"containers":[{"name":"api","image":"app:v1","env":[{"name":"Logging:LogLevel:Default","valueFrom":{"configMapKeyRef":{"name":"settings","key":"level"}}},{"name":".NET_SETTING","valueFrom":{"secretKeyRef":{"name":"credentials","key":"login"}}},{"name":"DUP","valueFrom":{"configMapKeyRef":{"name":"first","key":"a"}}},{"name":"DUP","valueFrom":{"configMapKeyRef":{"name":"second","key":"b"}}},{"name":"DUP\"][0]","valueFrom":{"configMapKeyRef":{"name":"distinct","key":"c"}}},{"name":"name\\with\"quotes]","valueFrom":{"configMapKeyRef":{"name":"escaped","key":"d"}}},{"name":"literal setting","value":"ENV_CANARY"}]}]}}`)
	before, gaps, err := ProjectTemplate(raw)
	if err != nil {
		t.Fatalf("valid environment discarded image/template: %v", err)
	}
	for path, want := range map[string]any{
		`spec.containers[api].env["Logging:LogLevel:Default"].valueFrom.configMapKeyRef.name`: "settings",
		`spec.containers[api].env[".NET_SETTING"].valueFrom.secretKeyRef.key`:                 "login",
		`spec.containers[api].env["DUP"][0].valueFrom.configMapKeyRef.name`:                   "first",
		`spec.containers[api].env["DUP"][1].valueFrom.configMapKeyRef.name`:                   "second",
		`spec.containers[api].env["DUP\"][0]"].valueFrom.configMapKeyRef.name`:                "distinct",
		`spec.containers[api].env["name\\with\"quotes]"].valueFrom.configMapKeyRef.name`:      "escaped",
	} {
		if before[path] != want {
			t.Errorf("%s = %v, want %v", path, before[path], want)
		}
	}
	if !strings.Contains(strings.Join(gaps, " "), "duplicate") {
		t.Fatalf("duplicate association coverage missing: %v", gaps)
	}
	after, _, err := ProjectTemplate(json.RawMessage(strings.Replace(string(raw), "app:v1", "app:v2", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := DiffTemplates(before, after); !reflect.DeepEqual(got, []FieldChange{{Path: "spec.containers[api].image", Before: "app:v1", After: "app:v2"}}) {
		t.Fatalf("image change lost or env identity collision: %+v", got)
	}
	output, _ := json.Marshal([]any{before, after, gaps})
	if strings.Contains(string(output), "ENV_CANARY") {
		t.Fatal("literal environment value escaped")
	}
}

func TestTemplateEnvironmentNamesDoNotRelaxOtherIdentities(t *testing.T) {
	for _, raw := range []string{
		`{"spec":{"containers":[{"name":"Logging:LogLevel:Default","image":"app:v1"}]}}`,
		`{"spec":{"containers":[{"name":"api","image":"app:v1"}],"volumes":[{"name":".NET_SETTING"}]}}`,
		`{"spec":{"containers":[{"name":"api","image":"app:v1","env":[{"name":"INVALID=NAME","value":"ENV_CANARY"}]}]}}`,
	} {
		_, _, err := ProjectTemplate(json.RawMessage(raw))
		if err == nil {
			t.Errorf("invalid identity accepted: %s", raw)
		}
	}
}

func TestTemplateProcessingCancellationAfterDecode(t *testing.T) {
	large := strings.Repeat("x", 6<<20)
	for _, field := range []string{"scalar", "dynamic key", "environment identity"} {
		t.Run(field, func(t *testing.T) {
			container := map[string]any{"name": "api", "image": "app:v1"}
			spec := map[string]any{"containers": []any{container}}
			switch field {
			case "scalar":
				container["image"] = large
			case "dynamic key":
				spec["nodeSelector"] = map[string]any{large: "batch"}
			case "environment identity":
				container["env"] = []any{map[string]any{"name": large, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "credentials", "key": "login"}}}}
			}
			raw, err := json.Marshal(map[string]any{"spec": spec})
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			// Start the budget after parsing so early reader cancellation cannot hide
			// an uninterruptible projection or identity-redaction path.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = projectTemplateField(ctx, decoded, templateSchema, "", map[string]any{}, map[string]bool{})
			elapsed := time.Since(start)
			if !errors.Is(err, context.DeadlineExceeded) || elapsed > 300*time.Millisecond {
				t.Fatalf("projection cancellation: elapsed=%s error=%v", elapsed, err)
			}
			t.Logf("elapsed=%s; cancellation observed after decode", elapsed)
		})
	}
}
