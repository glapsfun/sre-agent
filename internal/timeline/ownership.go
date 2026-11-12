package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/glapsfun/sre-agent/internal/safety"
)

var workloadKinds = map[string]string{
	"deployment": "Deployment", "deployments": "Deployment", "deploy": "Deployment",
	"statefulset": "StatefulSet", "statefulsets": "StatefulSet", "sts": "StatefulSet",
	"daemonset": "DaemonSet", "daemonsets": "DaemonSet", "ds": "DaemonSet",
	"job": "Job", "jobs": "Job",
	"cronjob": "CronJob", "cronjobs": "CronJob", "cj": "CronJob",
	"replicaset": "ReplicaSet", "replicasets": "ReplicaSet", "rs": "ReplicaSet",
	"pod": "Pod", "pods": "Pod", "po": "Pod",
}

var objectName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type ownedObject struct {
	Object
	Owners []ownerReference
}

func association(message string) Gap {
	return Gap{Source: "ownership", Code: "association", Message: message, Count: 1}
}

func ResolveScope(ctx context.Context, opts Options, kube KubeRunner) (Scope, []Gap, error) {
	scope := Scope{Context: opts.Context, Namespace: opts.Namespace, Workload: opts.Workload, Repo: opts.Repo, AlertmanagerURL: opts.AlertmanagerURL, Objects: map[string]Object{}, Related: map[string]bool{}}
	if kube != nil {
		scope.Context = kube.Context()
	}
	if !objectName.MatchString(opts.Namespace) {
		return scope, nil, errors.New("valid namespace is required")
	}
	if err := ctx.Err(); err != nil {
		return scope, nil, err
	}
	if opts.Workload == "" {
		return scope, nil, nil
	}
	kind, name := "", opts.Workload
	if parts := strings.Split(opts.Workload, "/"); len(parts) == 2 {
		kind = workloadKinds[strings.ToLower(parts[0])]
		name = parts[1]
		if kind == "" {
			return scope, nil, errors.New("unsupported workload kind")
		}
	} else if len(parts) != 1 {
		return scope, nil, errors.New("workload must be NAME or KIND/NAME")
	}
	if !objectName.MatchString(name) {
		return scope, nil, errors.New("invalid workload name")
	}
	var gaps []Gap
	graph := map[string]ownedObject{}
	err := ReadList(ctx, kube, "deployments.apps,statefulsets.apps,daemonsets.apps,jobs.batch,cronjobs.batch,replicasets.apps,pods", []string{"--namespace=" + opts.Namespace}, func(raw json.RawMessage) error {
		var object struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Namespace string           `json:"namespace"`
				Name      string           `json:"name"`
				UID       string           `json:"uid"`
				Owners    []ownerReference `json:"ownerReferences"`
			} `json:"metadata"`
		}
		if err := safety.DecodeJSON(ctx, string(raw), &object); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			gaps = append(gaps, association("invalid workload identity or owner metadata"))
			return nil
		}
		m := object.Metadata
		if workloadKinds[strings.ToLower(object.Kind)] != object.Kind || !validAPIVersion(object.Kind, object.APIVersion) || m.Namespace != opts.Namespace || !objectName.MatchString(m.Name) || m.UID == "" {
			gaps = append(gaps, association("invalid workload identity or namespace"))
			return nil
		}
		if _, exists := graph[m.UID]; exists {
			return errors.New("duplicate workload UID in Kubernetes list")
		}
		graph[m.UID] = ownedObject{Object: Object{APIVersion: object.APIVersion, Kind: object.Kind, Namespace: m.Namespace, Name: m.Name, UID: m.UID}, Owners: m.Owners}
		return nil
	})
	if err != nil {
		return scope, gaps, err
	}
	var matches []Object
	for _, object := range graph {
		if object.Name == name && (kind == "" || object.Kind == kind) {
			matches = append(matches, object.Object)
		}
	}
	if len(matches) == 0 {
		return scope, append(gaps, association("requested workload was not found")), errors.New("requested workload was not found")
	}
	if len(matches) > 1 {
		return scope, append(gaps, association("requested workload is ambiguous; specify KIND/NAME")), errors.New("requested workload is ambiguous; specify KIND/NAME")
	}
	scope.Target = &matches[0]
	keys := make([]string, 0, len(graph))
	for uid, object := range graph {
		scope.Objects[uid] = object.Object
		keys = append(keys, uid)
	}
	sort.Strings(keys)
	parents := map[string]string{}
	for _, uid := range keys {
		object := graph[uid]
		var controllers []ownerReference
		for _, owner := range object.Owners {
			if owner.Controller {
				controllers = append(controllers, owner)
			}
		}
		if len(controllers) == 0 {
			continue
		}
		if len(controllers) != 1 {
			gaps = append(gaps, association("multiple controller owners prevent exact association"))
			continue
		}
		owner := controllers[0]
		parent, exists := graph[owner.UID]
		if !exists || parent.Namespace != object.Namespace || parent.Kind != owner.Kind || parent.Name != owner.Name || apiGroup(parent.APIVersion) != apiGroup(owner.APIVersion) {
			gaps = append(gaps, association("controller owner is missing or has a conflicting identity"))
			continue
		}
		parents[uid] = owner.UID
	}
	// Validate cycles before treating the target as a terminal node, since a
	// malformed cycle can include that target too.
	checked := map[string]bool{}
	for _, uid := range keys {
		var path []string
		positions := map[string]int{}
		current := uid
		for current != "" && !checked[current] {
			if err := ctx.Err(); err != nil {
				return failedScope(scope), gaps, err
			}
			if at, cycle := positions[current]; cycle {
				gaps = append(gaps, association("controller ownership cycle prevents exact association"))
				for _, member := range path[at:] {
					delete(parents, member)
				}
				break
			}
			positions[current] = len(path)
			path = append(path, current)
			current = parents[current]
		}
		for _, member := range path {
			checked[member] = true
		}
	}
	// Resolve paths iteratively, memoizing outcomes to avoid recursion or repeated
	// traversal of long owner chains. Only descendants reach the selected UID.
	resolved := map[string]bool{scope.Target.UID: true}
	scope.Related[scope.Target.UID] = true
	for _, uid := range keys {
		if err := ctx.Err(); err != nil {
			return failedScope(scope), gaps, err
		}
		var path []string
		current := uid
		for current != "" && !resolved[current] {
			if err := ctx.Err(); err != nil {
				return failedScope(scope), gaps, err
			}
			path = append(path, current)
			current = parents[current]
		}
		included := resolved[current] && scope.Related[current]
		for _, member := range path {
			resolved[member] = true
			if included {
				scope.Related[member] = true
			}
		}
	}
	return scope, gaps, nil
}

func failedScope(scope Scope) Scope {
	scope.Target = nil
	scope.Objects = map[string]Object{}
	scope.Related = map[string]bool{}
	return scope
}

func validAPIVersion(kind, api string) bool {
	if kind == "Pod" {
		return api == "v1"
	}
	group := "apps/"
	if kind == "Job" || kind == "CronJob" {
		group = "batch/"
	}
	return strings.HasPrefix(api, group) && len(api) > len(group)
}

func apiGroup(version string) string {
	if group, _, found := strings.Cut(version, "/"); found {
		return group
	}
	return ""
}

// MatchObject never broadens a failed workload lookup to namespace scope. UID-less
// event references may use exact, unambiguous identity with a visible coverage gap.
func (s Scope) MatchObject(ref Object) (bool, *Gap) {
	if ref.Namespace != s.Namespace {
		return false, nil
	}
	if s.Workload == "" {
		return true, nil
	}
	if s.Target == nil {
		return false, nil
	}
	identity := func(object Object) bool {
		return object.Namespace == ref.Namespace && object.Kind == ref.Kind && object.Name == ref.Name && (ref.APIVersion == "" || apiGroup(object.APIVersion) == apiGroup(ref.APIVersion))
	}
	if ref.UID != "" {
		object, exists := s.Objects[ref.UID]
		return exists && s.Related[ref.UID] && identity(object), nil
	}
	var match Object
	count := 0
	for _, object := range s.Objects {
		if identity(object) {
			match = object
			count++
		}
	}
	if count != 1 || !s.Related[match.UID] {
		return false, nil
	}
	gap := association("object reference has no UID; used unambiguous namespace/kind/name association")
	return true, &gap
}
