package timeline

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

type gitOpsOutput struct {
	ctx    context.Context
	scope  Scope
	result SourceResult
	err    error
}

func newGitOpsOutput(ctx context.Context, scope Scope, source string) *gitOpsOutput {
	return &gitOpsOutput{ctx: ctx, scope: scope, result: newKubeResult(source)}
}
func (o *gitOpsOutput) text(value string) string {
	if o.err != nil {
		return ""
	}
	redacted, err := safety.NewRedactor().RedactContext(o.ctx, value)
	if err != nil {
		o.err = err
	}
	return redacted
}
func (o *gitOpsOutput) object(value Object) Object {
	return Object{APIVersion: o.text(value.APIVersion), Kind: o.text(value.Kind), Namespace: o.text(value.Namespace), Name: o.text(value.Name), UID: o.text(value.UID)}
}
func (o *gitOpsOutput) gap(code, message string) { sourceGap(&o.result, code, message) }
func (o *gitOpsOutput) context(object Object, details map[string]any) {
	if o.result.Summary.Context == nil {
		o.result.Summary.Context = map[string]any{"objects": []any{}}
	}
	details["object"] = o.object(object)
	o.result.Summary.Context["objects"] = append(o.result.Summary.Context["objects"].([]any), details)
}
func (o *gitOpsOutput) entry(object Object, suffix, stamp, basis, change, resource string, details map[string]any) {
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || t.IsZero() {
		o.gap("timestamp", "GitOps record has no usable "+basis+" timestamp")
		return
	}
	if !inWindow(o.scope, t) {
		return
	}
	o.result.Entries = append(o.result.Entries, Entry{ID: o.text(object.UID) + ":" + suffix, Time: t.UTC(), TimeBasis: basis, Source: o.result.Summary.Source, Object: o.object(object), Change: o.text(change), Command: "kubectl --context " + strconv.Quote(o.scope.Context) + " get " + resource + " --all-namespaces --chunk-size=500 -o json", Details: details})
}
func (o *gitOpsOutput) optionalTime(details map[string]any, key, value string) {
	if value == "" {
		return
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.IsZero() {
		o.gap("timestamp", "GitOps record has an invalid optional timestamp")
		return
	}
	details[key] = t.UTC()
}
func (o *gitOpsOutput) finish() SourceResult {
	if err := o.ctx.Err(); err != nil {
		return kubeFailure(o.result.Summary.Source, err)
	}
	if o.err != nil {
		return kubeFailure(o.result.Summary.Source, o.err)
	}
	return finishKubeResult(o.result)
}
func gitOpsFailure(source string, err error) SourceResult {
	result := kubeFailure(source, err)
	if result.Summary.Status == "missing" {
		result.Errors = []string{}
		result.Gaps[0].Message = "optional GitOps resource is not served by this cluster"
	}
	return result
}
func gitOpsIdentity(api, kind string, metadata kubeMetadata, wantKind, group string) (Object, bool) {
	obj := Object{APIVersion: api, Kind: kind, Namespace: metadata.Namespace, Name: metadata.Name, UID: metadata.UID}
	return obj, kind == wantKind && strings.HasPrefix(api, group+"/") && len(api) > len(group)+1 && objectName.MatchString(metadata.Namespace) && objectName.MatchString(metadata.Name) && metadata.UID != ""
}
func gitOpsMatches(scope Scope, group, kind, namespace, name string) bool {
	if namespace != scope.Namespace || kind == "" || !validEventReferenceName(name) {
		return false
	}
	if scope.Workload == "" {
		return true
	}
	if scope.Target == nil {
		return false
	}
	matches := func(obj Object) bool {
		return obj.Namespace == namespace && obj.Kind == kind && obj.Name == name && apiGroup(obj.APIVersion) == group
	}
	if matches(*scope.Target) {
		return true
	}
	for uid, obj := range scope.Objects {
		if scope.Related[uid] && matches(obj) {
			return true
		}
	}
	return false
}

type gitOpsCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

func (o *gitOpsOutput) conditions(object Object, conditions []gitOpsCondition, resource string) {
	for i, c := range conditions {
		if o.ctx.Err() != nil {
			return
		}
		if c.Type == "" || (c.Status != "True" && c.Status != "False" && c.Status != "Unknown") {
			o.gap("error", "invalid GitOps condition fields; condition omitted")
			continue
		}
		details := map[string]any{"type": o.text(c.Type), "status": o.text(c.Status), "reason": o.text(c.Reason), "message": o.text(c.Message), "observed_generation": c.ObservedGeneration}
		if t, e := time.Parse(time.RFC3339Nano, c.LastTransitionTime); e != nil || t.IsZero() {
			o.context(object, map[string]any{"undated_condition": details})
		}
		o.entry(object, "condition:"+strconv.Itoa(i), c.LastTransitionTime, "condition.lastTransitionTime", "condition "+c.Type+"="+c.Status, resource, details)
	}
}

type fluxInventory struct {
	Entries []struct {
		ID      string `json:"id"`
		Version string `json:"v"`
	} `json:"entries"`
}
type fluxRecord struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   kubeMetadata `json:"metadata"`
	Spec       struct {
		KubeConfig      json.RawMessage `json:"kubeConfig"`
		TargetNamespace string          `json:"targetNamespace"`
	} `json:"spec"`
	Status struct {
		Inventory             *fluxInventory    `json:"inventory"`
		Conditions            []gitOpsCondition `json:"conditions"`
		LastAppliedRevision   string            `json:"lastAppliedRevision"`
		LastAttemptedRevision string            `json:"lastAttemptedRevision"`
		History               []json.RawMessage `json:"history"`
	} `json:"status"`
}

func fluxAssociation(o *gitOpsOutput, record fluxRecord) bool {
	if raw := strings.TrimSpace(string(record.Spec.KubeConfig)); raw != "" && raw != "null" {
		o.gap("association", "Flux manager uses an unverifiable kubeConfig destination")
		return false
	}
	if record.Status.Inventory != nil {
		matched := false
		for _, ref := range record.Status.Inventory.Entries {
			parts := strings.Split(ref.ID, "_")
			if len(parts) != 4 || parts[1] == "" || parts[3] == "" || ref.Version == "" {
				o.gap("association", "Flux inventory contains an invalid or ambiguous resource identity")
				continue
			}
			if gitOpsMatches(o.scope, parts[2], parts[3], parts[0], parts[1]) {
				matched = true
			}
		}
		return matched
	}
	if record.Metadata.Namespace == o.scope.Namespace && (record.Spec.TargetNamespace == "" || record.Spec.TargetNamespace == o.scope.Namespace) && (o.scope.Workload == "" || o.scope.Target != nil && record.Metadata.Name == o.scope.Target.Name) {
		o.gap("association", "Flux inventory is unavailable; association uses exact manager namespace/name fallback")
		return true
	}
	o.gap("association", "Flux inventory is unavailable and no exact manager namespace/name association is established")
	return false
}

func CollectFluxKustomizations(ctx context.Context, scope Scope, kube KubeRunner) SourceResult {
	return collectFlux(ctx, scope, kube, "flux-kustomizations", "Kustomization", "kustomize.toolkit.fluxcd.io", "kustomizations.kustomize.toolkit.fluxcd.io")
}
func CollectFluxHelmReleases(ctx context.Context, scope Scope, kube KubeRunner) SourceResult {
	return collectFlux(ctx, scope, kube, "flux-helmreleases", "HelmRelease", "helm.toolkit.fluxcd.io", "helmreleases.helm.toolkit.fluxcd.io")
}
func collectFlux(ctx context.Context, scope Scope, kube KubeRunner, source, kind, group, resource string) SourceResult {
	o := newGitOpsOutput(ctx, scope, source)
	err := ReadList(ctx, kube, resource, []string{"--all-namespaces", "--chunk-size=500"}, func(raw json.RawMessage) error {
		o.result.Summary.Observed++
		var record fluxRecord
		if err := safety.DecodeJSON(ctx, string(raw), &record); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			o.gap("error", "invalid Flux resource fields; record omitted")
			return nil
		}
		object, valid := gitOpsIdentity(record.APIVersion, record.Kind, record.Metadata, kind, group)
		if !valid {
			o.gap("error", "invalid Flux resource identity; record omitted")
			return nil
		}
		if !fluxAssociation(o, record) {
			return ctx.Err()
		}
		o.result.Summary.Matched++
		current := map[string]any{}
		if record.Status.LastAppliedRevision != "" && kind == "Kustomization" {
			current["last_applied_revision"] = o.text(record.Status.LastAppliedRevision)
			o.gap("timestamp", "Flux lastAppliedRevision has no application timestamp; retained as undated context")
		}
		if record.Status.LastAttemptedRevision != "" {
			current["last_attempted_revision"] = o.text(record.Status.LastAttemptedRevision)
		}
		if len(current) > 0 {
			o.context(object, current)
		}
		o.conditions(object, record.Status.Conditions, resource)
		for i, history := range record.Status.History {
			if err := ctx.Err(); err != nil {
				return err
			}
			if kind == "Kustomization" {
				o.kustomizationHistory(object, history, i, resource)
			} else {
				o.helmHistory(object, history, i, resource)
			}
		}
		o.gap("history", "only retained GitOps history is available; earlier revisions and historical resource associations may be absent")
		return o.err
	})
	if err != nil {
		return gitOpsFailure(source, err)
	}
	return o.finish()
}

func (o *gitOpsOutput) kustomizationHistory(object Object, raw json.RawMessage, index int, resource string) {
	var h struct {
		First    string `json:"firstReconciled"`
		Last     string `json:"lastReconciled"`
		Status   string `json:"lastReconciledStatus"`
		Count    int64  `json:"totalReconciliations"`
		Metadata struct {
			Revision string `json:"revision"`
		} `json:"metadata"`
	}
	if safety.DecodeJSON(o.ctx, string(raw), &h) != nil || h.Status == "" || h.Count < 1 {
		o.gap("error", "invalid Kustomization history record; record omitted")
		return
	}
	details := map[string]any{"status": o.text(h.Status), "total_reconciliations": h.Count, "revision": o.text(h.Metadata.Revision)}
	o.optionalTime(details, "first_reconciled", h.First)
	if t, e := time.Parse(time.RFC3339Nano, h.Last); e != nil || t.IsZero() {
		o.context(object, map[string]any{"undated_history": details})
	}
	o.entry(object, "history:"+strconv.Itoa(index), h.Last, "history.lastReconciled", "Kustomization reconciliation "+h.Status, resource, details)
}
func (o *gitOpsOutput) helmHistory(object Object, raw json.RawMessage, index int, resource string) {
	var h struct {
		Name         string `json:"name"`
		Namespace    string `json:"namespace"`
		Version      int64  `json:"version"`
		ChartName    string `json:"chartName"`
		ChartVersion string `json:"chartVersion"`
		AppVersion   string `json:"appVersion"`
		Status       string `json:"status"`
		First        string `json:"firstDeployed"`
		Last         string `json:"lastDeployed"`
	}
	if safety.DecodeJSON(o.ctx, string(raw), &h) != nil || h.Version < 1 || h.Status == "" {
		o.gap("error", "invalid HelmRelease history record; record omitted")
		return
	}
	details := map[string]any{"name": o.text(h.Name), "namespace": o.text(h.Namespace), "version": h.Version, "chart_name": o.text(h.ChartName), "chart_version": o.text(h.ChartVersion), "app_version": o.text(h.AppVersion), "status": o.text(h.Status)}
	o.optionalTime(details, "first_deployed", h.First)
	if t, e := time.Parse(time.RFC3339Nano, h.Last); e != nil || t.IsZero() {
		o.context(object, map[string]any{"undated_history": details})
	}
	o.entry(object, "history:"+strconv.Itoa(index), h.Last, "history.lastDeployed", "Helm release "+h.Status, resource, details)
}

type argoHistory struct {
	ID              int64    `json:"id"`
	Revision        string   `json:"revision"`
	Revisions       []string `json:"revisions"`
	DeployedAt      string   `json:"deployedAt"`
	DeployStartedAt string   `json:"deployStartedAt"`
}
type argoRecord struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   kubeMetadata `json:"metadata"`
	Spec       struct {
		Destination struct {
			Server    string `json:"server"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status struct {
		ControllerNamespace string `json:"controllerNamespace"`
		Resources           []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"resources"`
		History []argoHistory `json:"history"`
	} `json:"status"`
}

func gitOpsServer(server string) string {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}
func (o *gitOpsOutput) auxiliary(result executil.Result, description string) bool {
	if result.Err == nil {
		return true
	}
	failure := kubeFailure(o.result.Summary.Source, &ProducerError{Stderr: result.Stderr, ExitCode: result.ExitCode, cause: result.Err})
	o.gap(failure.Summary.Status, description)
	o.result.Errors = append(o.result.Errors, failure.Errors...)
	return false
}

func CollectArgo(ctx context.Context, scope Scope, kube KubeRunner) SourceResult {
	const resource = "applications.argoproj.io"
	o := newGitOpsOutput(ctx, scope, "argocd")
	var apps []argoRecord
	err := ReadList(ctx, kube, resource, []string{"--all-namespaces", "--chunk-size=500"}, func(raw json.RawMessage) error {
		o.result.Summary.Observed++
		var app argoRecord
		if err := safety.DecodeJSON(ctx, string(raw), &app); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			o.gap("error", "invalid Argo Application fields; record omitted")
			return nil
		}
		if _, valid := gitOpsIdentity(app.APIVersion, app.Kind, app.Metadata, "Application", "argoproj.io"); !valid {
			o.gap("error", "invalid Argo Application identity; record omitted")
			return nil
		}
		// Only typed revision, identity and destination fields survive the list read.
		app.Metadata.Annotations = nil
		apps = append(apps, app)
		return ctx.Err()
	})
	if err != nil {
		return gitOpsFailure("argocd", err)
	}
	if scope.Workload != "" && scope.Target == nil {
		o.gap("association", "requested workload is unresolved; Argo association is unavailable")
		return o.finish()
	}
	if len(apps) == 0 {
		return o.finish()
	}
	serverResult := kube.Run(ctx, "config", "view", "--minify", "--output=jsonpath={.clusters[0].cluster.server}")
	server := ""
	if o.auxiliary(serverResult, "pinned cluster server could not be read for Argo destination verification") {
		server = gitOpsServer(strings.TrimSpace(serverResult.Stdout))
		if server == "" {
			o.gap("association", "pinned cluster server is missing or invalid")
		}
	}
	eligible := make([]bool, len(apps))
	members := make([]bool, len(apps))
	independent := make([]bool, len(apps))
	claims := map[[4]string][]int{}
	memberCount, eligibleCount := 0, 0
	missingResources := false
	for i, app := range apps {
		if err := ctx.Err(); err != nil {
			return kubeFailure("argocd", err)
		}
		destination := app.Spec.Destination
		candidate := gitOpsServer(destination.Server)
		if destination.Name != "" || candidate == "" || (candidate != "https://kubernetes.default.svc" && (server == "" || candidate != server)) {
			o.gap("association", "Argo destination is remote, symbolic or unverifiable")
			continue
		}
		eligible[i] = true
		eligibleCount++
		missingResources = missingResources || len(app.Status.Resources) == 0
		seen := map[[4]string]bool{}
		for _, ref := range app.Status.Resources {
			if gitOpsMatches(scope, ref.Group, ref.Kind, ref.Namespace, ref.Name) {
				members[i] = true
				key := [4]string{ref.Group, ref.Kind, ref.Namespace, ref.Name}
				if !seen[key] {
					claims[key] = append(claims[key], i)
					seen[key] = true
				}
			}
		}
		if members[i] {
			memberCount++
		}
	}
	conflict := false
	for _, indexes := range claims {
		if len(indexes) == 1 {
			independent[indexes[0]] = true
		} else {
			conflict = true
			o.gap("association", "multiple Argo Applications claim the same resource identity")
		}
	}
	// Separate descendants can have separate managers. Only overlapping claims
	// need tracking proof; an independent membership remains usable if it fails.
	var tracking *argoTracking
	if scope.Workload != "" && eligibleCount > 0 && (memberCount == 0 || conflict || missingResources) {
		tracking = readArgoTracking(o, kube)
	}
	installation := map[string]struct {
		value string
		known bool
	}{}
	tracked := make([]bool, len(apps))
	trackedCount := 0
	if tracking != nil {
		for i, app := range apps {
			if ctx.Err() != nil {
				return kubeFailure("argocd", ctx.Err())
			}
			if !eligible[i] {
				continue
			}
			controller := app.Status.ControllerNamespace
			if !objectName.MatchString(controller) {
				o.gap("association", "Argo controller namespace is unknown; installation identity cannot be verified")
				continue
			}
			instanceName := app.Metadata.Name
			if app.Metadata.Namespace != controller {
				instanceName = app.Metadata.Namespace + "_" + instanceName
			}
			if instanceName != tracking.application {
				continue
			}
			identity, ok := installation[controller]
			if !ok {
				response := kube.Run(ctx, "get", "configmap", "argocd-cm", "--namespace="+controller, "--output=jsonpath={.data.installationID}")
				identity.known = o.auxiliary(response, "Argo installation ID lookup failed")
				identity.value = response.Stdout
				installation[controller] = identity
			}
			if identity.known && identity.value == tracking.installation {
				tracked[i] = true
				trackedCount++
			} else {
				o.gap("association", "Argo tracking installation identity is mismatched or unverifiable")
			}
		}
	}
	for i, app := range apps {
		matched := members[i]
		if scope.Workload != "" && !independent[i] {
			matched = trackedCount == 1 && tracked[i]
		}
		if !eligible[i] || !matched {
			if eligible[i] {
				o.gap("association", "Argo Application has no unambiguous exact resource association")
			}
			continue
		}
		o.result.Summary.Matched++
		object, _ := gitOpsIdentity(app.APIVersion, app.Kind, app.Metadata, "Application", "argoproj.io")
		for index, h := range app.Status.History {
			if ctx.Err() != nil {
				return kubeFailure("argocd", ctx.Err())
			}
			valid := h.ID >= 0 && (strings.TrimSpace(h.Revision) != "" || len(h.Revisions) > 0)
			for _, revision := range h.Revisions {
				valid = valid && strings.TrimSpace(revision) != ""
			}
			if !valid {
				o.gap("error", "invalid Argo history identity or revisions; record omitted")
				continue
			}
			revisions := make([]string, len(h.Revisions))
			for j, v := range h.Revisions {
				revisions[j] = o.text(v)
			}
			details := map[string]any{"history_id": h.ID, "revision": o.text(h.Revision), "revisions": revisions}
			o.optionalTime(details, "deploy_started_at", h.DeployStartedAt)
			if t, e := time.Parse(time.RFC3339Nano, h.DeployedAt); e != nil || t.IsZero() {
				o.context(object, map[string]any{"undated_history": details})
			}
			o.entry(object, "history:"+strconv.FormatInt(h.ID, 10)+":"+strconv.Itoa(index), h.DeployedAt, "history.deployedAt", "Argo sync completed", resource, details)
		}
		o.gap("history", "only retained Argo sync history is available; pruned revisions and past resource associations may be absent")
	}
	return o.finish()
}

type argoTracking struct{ application, installation string }

func readArgoTracking(o *gitOpsOutput, kube KubeRunner) *argoTracking {
	target := o.scope.Target
	if target == nil {
		return nil
	}
	resource := strings.ToLower(target.Kind)
	if group := apiGroup(target.APIVersion); group != "" {
		resource += "." + group
	}
	response := kube.Run(o.ctx, "get", resource, target.Name, "--namespace="+target.Namespace, "--output=json")
	if !o.auxiliary(response, "selected workload metadata lookup failed for Argo tracking") {
		return nil
	}
	var live struct {
		APIVersion string       `json:"apiVersion"`
		Kind       string       `json:"kind"`
		Metadata   kubeMetadata `json:"metadata"`
	}
	if safety.DecodeJSON(o.ctx, response.Stdout, &live) != nil || live.Metadata.UID != target.UID || live.Metadata.Name != target.Name || live.Metadata.Namespace != target.Namespace || live.Kind != target.Kind || apiGroup(live.APIVersion) != apiGroup(target.APIVersion) {
		o.gap("association", "selected workload metadata identity changed or is invalid")
		return nil
	}
	value := live.Metadata.Annotations["argocd.argoproj.io/tracking-id"]
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] != apiGroup(target.APIVersion)+"/"+target.Kind || parts[2] != target.Namespace+"/"+target.Name {
		o.gap("association", "Argo tracking annotation is missing or does not reference the selected object")
		return nil
	}
	return &argoTracking{application: parts[0], installation: live.Metadata.Annotations["argocd.argoproj.io/installation-id"]}
}
