package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

type kubeMetadata struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	UID         string            `json:"uid"`
	Created     string            `json:"creationTimestamp"`
	Owners      []ownerReference  `json:"ownerReferences"`
	Annotations map[string]string `json:"annotations"`
}

func newKubeResult(source string) SourceResult {
	return SourceResult{Summary: SourceSummary{Source: source, Status: "ok"}, Entries: []Entry{}, Gaps: []Gap{}, Errors: []string{}}
}
func sourceGap(result *SourceResult, code, message string) {
	for i := range result.Gaps {
		g := &result.Gaps[i]
		if g.Code == code && g.Message == message {
			g.Count++
			return
		}
	}
	result.Gaps = append(result.Gaps, Gap{Source: result.Summary.Source, Code: code, Message: message, Count: 1})
}
func finishKubeResult(result SourceResult) SourceResult {
	if len(result.Gaps) > 0 && result.Summary.Status == "ok" {
		result.Summary.Status = "partial"
	}
	return result
}
func kubeFailure(source string, err error) SourceResult {
	result := newKubeResult(source)
	code := "error"
	var producer *ProducerError
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		code = "timeout"
	} else if errors.As(err, &producer) {
		diagnostic := strings.ToLower(producer.Stderr)
		switch {
		case strings.Contains(diagnostic, "forbidden"), strings.Contains(diagnostic, "unauthorized"):
			code = "denied"
		case strings.Contains(diagnostic, "doesn't have a resource type"), strings.Contains(diagnostic, "could not find the requested resource"), strings.Contains(diagnostic, "(notfound)"):
			code = "missing"
		}
	}
	result.Summary.Status = code
	sourceGap(&result, code, "Kubernetes source retrieval failed; no records from this response were retained")
	message := safety.NewRedactor().Redact(err.Error())
	result.Errors = append(result.Errors, message)
	return result
}
func inWindow(scope Scope, t time.Time) bool { return !t.Before(scope.Start) && !t.After(scope.End) }
func kubeCommand(scope Scope, resource string) string {
	return "kubectl --context " + strconv.Quote(scope.Context) + " get " + resource + " --namespace " + strconv.Quote(scope.Namespace) + " --chunk-size=500 -o json"
}

func validEventReferenceName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/%")
}

func CollectEvents(ctx context.Context, scope Scope, kube KubeRunner) SourceResult {
	result := newKubeResult("kubernetes-events")
	err := ReadList(ctx, kube, "events", []string{"--namespace=" + scope.Namespace, "--chunk-size=500"}, func(raw json.RawMessage) error {
		result.Summary.Observed++
		var event struct {
			APIVersion string       `json:"apiVersion"`
			Kind       string       `json:"kind"`
			Metadata   kubeMetadata `json:"metadata"`
			Involved   struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Namespace  string `json:"namespace"`
				Name       string `json:"name"`
				UID        string `json:"uid"`
			} `json:"involvedObject"`
			Type      string `json:"type"`
			Reason    string `json:"reason"`
			Message   string `json:"message"`
			First     string `json:"firstTimestamp"`
			Last      string `json:"lastTimestamp"`
			EventTime string `json:"eventTime"`
			Count     int64  `json:"count"`
			Series    *struct {
				Last  string `json:"lastObservedTime"`
				Count int64  `json:"count"`
			} `json:"series"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Kind != "Event" || event.APIVersion != "v1" || event.Metadata.Namespace != scope.Namespace || event.Metadata.UID == "" || !objectName.MatchString(event.Metadata.Name) || event.Involved.Kind == "" || !validEventReferenceName(event.Involved.Name) {
			sourceGap(&result, "error", "invalid event identity or fields; record omitted")
			return nil
		}
		ref := Object{APIVersion: event.Involved.APIVersion, Kind: event.Involved.Kind, Namespace: event.Involved.Namespace, Name: event.Involved.Name, UID: event.Involved.UID}
		// Namespace-wide events belong to their metadata namespace, including
		// events about cluster objects whose reference namespace is empty.
		if scope.Workload != "" {
			matched, gap := scope.MatchObject(ref)
			if !matched {
				return nil
			}
			if gap != nil {
				sourceGap(&result, gap.Code, gap.Message)
			}
		}
		result.Summary.Matched++
		candidates := []struct{ basis, value string }{{"lastTimestamp", event.Last}, {"eventTime", event.EventTime}, {"firstTimestamp", event.First}, {"metadata.creationTimestamp", event.Metadata.Created}}
		if event.Series != nil {
			candidates = append([]struct{ basis, value string }{{"series.lastObservedTime", event.Series.Last}}, candidates...)
		}
		var stamp time.Time
		basis := ""
		invalid := false
		for _, candidate := range candidates {
			if candidate.value == "" {
				continue
			}
			parsed, e := time.Parse(time.RFC3339Nano, candidate.value)
			if e != nil || parsed.IsZero() {
				invalid = true
				continue
			}
			if stamp.IsZero() {
				stamp = parsed.UTC()
				basis = candidate.basis
			}
		}
		if stamp.IsZero() {
			sourceGap(&result, "timestamp", "event has no usable observation timestamp")
			return nil
		}
		if invalid {
			sourceGap(&result, "timestamp", "event has an invalid timestamp; retained usable endpoints and selected a documented fallback")
		}
		if !inWindow(scope, stamp) {
			first, e := time.Parse(time.RFC3339Nano, event.First)
			if e == nil && !first.After(scope.End) && stamp.After(scope.End) {
				sourceGap(&result, "timestamp", "aggregate event spans the window but individual occurrence times are unavailable")
			}
			return nil
		}
		redact := safety.NewRedactor()
		details := map[string]any{"count": event.Count}
		for _, field := range []struct{ name, value string }{{"type", event.Type}, {"reason", event.Reason}, {"message", event.Message}} {
			clean, err := redact.RedactContext(ctx, field.value)
			if err != nil {
				return err
			}
			details[field.name] = clean
		}
		change, err := redact.RedactContext(ctx, event.Reason+": "+event.Message)
		if err != nil {
			return err
		}
		// Keep observed endpoints, not synthesized occurrences for aggregate counts.
		for _, candidate := range candidates {
			if parsed, e := time.Parse(time.RFC3339Nano, candidate.value); e == nil && !parsed.IsZero() {
				details[candidate.basis] = parsed.UTC()
			}
		}
		if event.Series != nil {
			details["series.count"] = event.Series.Count
		}
		result.Entries = append(result.Entries, Entry{ID: event.Metadata.UID, Time: stamp, TimeBasis: basis, Source: result.Summary.Source, Object: ref, Change: change, Command: kubeCommand(scope, "events"), Details: details})
		return ctx.Err()
	})
	if err != nil {
		return kubeFailure(result.Summary.Source, err)
	}
	sourceGap(&result, "history", "only retained Kubernetes event records are available; aggregate counts do not provide individual occurrence times")
	return finishKubeResult(result)
}

type retainedReplicaSet struct {
	object     Object
	deployment Object
	created    time.Time
	revision   int64
	template   map[string]any
	reused     bool
}

func CollectReplicaSets(ctx context.Context, scope Scope, kube KubeRunner) SourceResult {
	result := newKubeResult("replicasets")
	if scope.Workload != "" && (scope.Target == nil || scope.Target.Kind != "Deployment") {
		sourceGap(&result, "history", "ReplicaSet revision history requires a resolved Deployment target")
		return finishKubeResult(result)
	}
	groups := map[string][]retainedReplicaSet{}
	owners := map[string]Object{}
	err := ReadList(ctx, kube, "replicasets.apps", []string{"--namespace=" + scope.Namespace, "--chunk-size=500"}, func(raw json.RawMessage) error {
		result.Summary.Observed++
		var rs struct {
			APIVersion string       `json:"apiVersion"`
			Kind       string       `json:"kind"`
			Metadata   kubeMetadata `json:"metadata"`
			Spec       struct {
				Template json.RawMessage `json:"template"`
			} `json:"spec"`
		}
		if json.Unmarshal(raw, &rs) != nil || rs.Kind != "ReplicaSet" || !validAPIVersion(rs.Kind, rs.APIVersion) || rs.Metadata.Namespace != scope.Namespace || rs.Metadata.UID == "" || !objectName.MatchString(rs.Metadata.Name) {
			sourceGap(&result, "error", "invalid ReplicaSet identity or fields; record omitted")
			return nil
		}
		object := Object{APIVersion: rs.APIVersion, Kind: rs.Kind, Namespace: rs.Metadata.Namespace, Name: rs.Metadata.Name, UID: rs.Metadata.UID}
		matched, _ := scope.MatchObject(object)
		if !matched {
			return nil
		}
		var controllers []ownerReference
		for _, owner := range rs.Metadata.Owners {
			if owner.Controller {
				controllers = append(controllers, owner)
			}
		}
		if len(controllers) != 1 || controllers[0].Kind != "Deployment" || apiGroup(controllers[0].APIVersion) != "apps" || controllers[0].UID == "" || !objectName.MatchString(controllers[0].Name) {
			sourceGap(&result, "association", "ReplicaSet has no unique Deployment controller identity")
			return nil
		}
		owner := controllers[0]
		deployment := Object{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: rs.Metadata.Namespace, Name: owner.Name, UID: owner.UID}
		if scope.Target != nil && (scope.Target.UID != owner.UID || scope.Target.Name != owner.Name || apiGroup(scope.Target.APIVersion) != apiGroup(owner.APIVersion)) {
			sourceGap(&result, "association", "ReplicaSet controller conflicts with the selected Deployment")
			return nil
		}
		if previous, ok := owners[owner.UID]; ok && previous.Name != owner.Name {
			return errors.New("conflicting Deployment identities in ReplicaSet source")
		}
		owners[owner.UID] = deployment
		result.Summary.Matched++
		projected, excluded, e := projectTemplate(ctx, rs.Spec.Template)
		if e != nil {
			if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
				return e
			}
			sourceGap(&result, "error", "invalid ReplicaSet pod template; record omitted")
			return nil
		}
		for _, path := range excluded {
			sourceGap(&result, "redacted", "pod template coverage excludes "+path)
		}
		created, e := time.Parse(time.RFC3339Nano, rs.Metadata.Created)
		if e != nil || created.IsZero() {
			sourceGap(&result, "timestamp", "ReplicaSet has no usable creation timestamp")
			return nil
		}
		rev, e := strconv.ParseInt(rs.Metadata.Annotations["deployment.kubernetes.io/revision"], 10, 64)
		if e != nil || rev <= 0 {
			rev = 0
			sourceGap(&result, "history", "ReplicaSet has no valid numeric revision; predecessor unavailable")
		}
		groups[owner.UID] = append(groups[owner.UID], retainedReplicaSet{object: object, deployment: deployment, created: created.UTC(), revision: rev, template: projected, reused: rs.Metadata.Annotations["deployment.kubernetes.io/revision-history"] != ""})
		return ctx.Err()
	})
	if err != nil {
		return kubeFailure(result.Summary.Source, err)
	}
	groupIDs := make([]string, 0, len(groups))
	for uid := range groups {
		groupIDs = append(groupIDs, uid)
	}
	sort.Strings(groupIDs)
	for _, uid := range groupIDs {
		group := groups[uid]
		sort.Slice(group, func(i, j int) bool {
			if group[i].revision != group[j].revision {
				return group[i].revision < group[j].revision
			}
			return group[i].object.UID < group[j].object.UID
		})
		counts := map[int64]int{}
		for _, rs := range group {
			counts[rs.revision]++
		}
		var previous *retainedReplicaSet
		for i := range group {
			if err := ctx.Err(); err != nil {
				return kubeFailure(result.Summary.Source, err)
			}
			rs := &group[i]
			details := map[string]any{"deployment": rs.deployment, "revision": rs.revision, "template": rs.template, "baseline_available": false}
			if rs.revision > 0 && counts[rs.revision] == 1 && previous != nil {
				details["baseline_available"] = true
				details["previous_revision"] = previous.revision
				details["previous_replicaset"] = previous.object
				details["changes"] = DiffTemplates(previous.template, rs.template)
				if rs.revision-previous.revision != 1 {
					sourceGap(&result, "history", "retained ReplicaSet revisions have missing intermediate history")
				}
				if !rs.created.After(previous.created) {
					sourceGap(&result, "timestamp", "ReplicaSet revision order differs from creation order; revision application time is unknown")
				}
			} else {
				sourceGap(&result, "history", "ReplicaSet predecessor is unavailable or revision identity is ambiguous")
			}
			if rs.reused {
				sourceGap(&result, "history", "ReplicaSet revision history indicates reuse; intermediate templates are unavailable")
				sourceGap(&result, "timestamp", "reused ReplicaSet creation time does not date the later revision")
			}
			if inWindow(scope, rs.created) {
				result.Entries = append(result.Entries, Entry{ID: rs.object.UID, Time: rs.created, TimeBasis: "replicaset-created", Source: result.Summary.Source, Object: rs.object, Change: "ReplicaSet created; retained template revision " + strconv.FormatInt(rs.revision, 10), Command: kubeCommand(scope, "replicasets.apps"), Details: details})
			}
			if rs.revision > 0 {
				if counts[rs.revision] == 1 {
					previous = rs
				} else {
					previous = nil
				}
			}
		}
	}
	sourceGap(&result, "history", "only retained ReplicaSet templates are available; deleted revisions and rollback reuse may be absent")
	sourceGap(&result, "timestamp", "ReplicaSet creation timestamps do not establish revision application or rollout completion times")
	return finishKubeResult(result)
}
