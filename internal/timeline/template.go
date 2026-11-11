package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/glapsfun/sre-agent/internal/safety"
)

type FieldChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// Template fields are allowlisted recursively. Unknown fields never reach retained
// data, including diagnostics, where only schema paths are reported.
type templateField struct {
	fields      map[string]*templateField
	item        *templateField
	named       bool
	environment bool
	scalar      string
	dynamic     bool
	ignore      bool
}

var textField = &templateField{scalar: "string"}
var numberField = &templateField{scalar: "number"}
var boolField = &templateField{scalar: "bool"}

func fields(names string, field *templateField) map[string]*templateField {
	result := map[string]*templateField{}
	for _, name := range strings.Fields(names) {
		result[name] = field
	}
	return result
}
func objectField(groups ...map[string]*templateField) *templateField {
	f := &templateField{fields: map[string]*templateField{}}
	for _, group := range groups {
		for k, v := range group {
			f.fields[k] = v
		}
	}
	return f
}
func arrayField(item *templateField, named bool) *templateField {
	return &templateField{item: item, named: named}
}
func stringMap() *templateField { return &templateField{item: textField, dynamic: true} }

var templateSchema = buildTemplateSchema()
var templateItemName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func buildTemplateSchema() *templateField {
	reference := objectField(fields("name key", textField), fields("optional", boolField))
	selectorRequirement := objectField(fields("key operator", textField), map[string]*templateField{"values": arrayField(textField, false)})
	selector := objectField(map[string]*templateField{"matchLabels": stringMap(), "matchExpressions": arrayField(selectorRequirement, false)})
	nodeTerm := objectField(map[string]*templateField{"matchExpressions": arrayField(selectorRequirement, false), "matchFields": arrayField(selectorRequirement, false)})
	podTerm := objectField(fields("topologyKey", textField), map[string]*templateField{"labelSelector": selector, "namespaceSelector": selector, "namespaces": arrayField(textField, false), "matchLabelKeys": arrayField(textField, false), "mismatchLabelKeys": arrayField(textField, false)})
	podAffinity := objectField(map[string]*templateField{"requiredDuringSchedulingIgnoredDuringExecution": arrayField(podTerm, false), "preferredDuringSchedulingIgnoredDuringExecution": arrayField(objectField(map[string]*templateField{"weight": numberField, "podAffinityTerm": podTerm}), false)})
	security := objectField(fields("runAsUser runAsGroup fsGroup", numberField), fields("runAsNonRoot privileged readOnlyRootFilesystem allowPrivilegeEscalation", boolField), fields("procMount fsGroupChangePolicy supplementalGroupsPolicy", textField), map[string]*templateField{
		"capabilities":   objectField(map[string]*templateField{"add": arrayField(textField, false), "drop": arrayField(textField, false)}),
		"seccompProfile": objectField(fields("type localhostProfile", textField)), "appArmorProfile": objectField(fields("type localhostProfile", textField)),
		"seLinuxOptions": objectField(fields("user role type level", textField)), "supplementalGroups": arrayField(numberField, false),
		"windowsOptions": objectField(fields("runAsUserName gmsaCredentialSpecName", textField), fields("hostProcess", boolField)),
	})
	// Probe ports are IntOrString in the API.
	port := &templateField{scalar: "port"}
	httpGet := objectField(fields("host path scheme", textField), map[string]*templateField{"port": port, "httpHeaders": nil})
	action := objectField(map[string]*templateField{"exec": nil, "httpGet": httpGet, "tcpSocket": objectField(fields("host", textField), map[string]*templateField{"port": port}), "sleep": objectField(fields("seconds", numberField))})
	probe := objectField(fields("initialDelaySeconds timeoutSeconds periodSeconds successThreshold failureThreshold terminationGracePeriodSeconds", numberField), map[string]*templateField{"exec": nil, "httpGet": httpGet, "tcpSocket": action.fields["tcpSocket"], "grpc": objectField(fields("service", textField), fields("port", numberField))})
	resource := objectField(map[string]*templateField{"limits": stringMap(), "requests": stringMap(), "claims": arrayField(objectField(fields("name request", textField)), false)})
	fieldRef := objectField(fields("apiVersion fieldPath", textField))
	resourceRef := objectField(fields("containerName resource divisor", textField))
	env := objectField(fields("name", textField), map[string]*templateField{"value": nil, "valueFrom": objectField(map[string]*templateField{"secretKeyRef": reference, "configMapKeyRef": reference, "fieldRef": fieldRef, "resourceFieldRef": resourceRef})})
	container := objectField(fields("name image imagePullPolicy workingDir terminationMessagePath terminationMessagePolicy restartPolicy", textField), fields("stdin stdinOnce tty", boolField), map[string]*templateField{
		"command": nil, "args": nil, "env": &templateField{item: env, environment: true}, "envFrom": arrayField(objectField(fields("prefix", textField), map[string]*templateField{"secretRef": reference, "configMapRef": reference}), false),
		"resources": resource, "ports": arrayField(objectField(fields("name protocol hostIP", textField), fields("containerPort hostPort", numberField)), false),
		"livenessProbe": probe, "readinessProbe": probe, "startupProbe": probe, "securityContext": security,
		"lifecycle":     objectField(map[string]*templateField{"postStart": action, "preStop": action}),
		"volumeMounts":  arrayField(objectField(fields("name mountPath subPath subPathExpr mountPropagation recursiveReadOnly", textField), fields("readOnly", boolField)), false),
		"volumeDevices": arrayField(objectField(fields("name devicePath", textField)), false),
		"resizePolicy":  arrayField(objectField(fields("resourceName restartPolicy", textField)), false),
	})
	keyPath := objectField(fields("key path", textField), fields("mode", numberField))
	configVolume := objectField(fields("name secretName", textField), fields("optional", boolField), fields("defaultMode", numberField), map[string]*templateField{"items": arrayField(keyPath, false)})
	downward := objectField(fields("defaultMode", numberField), map[string]*templateField{"items": arrayField(objectField(fields("path", textField), fields("mode", numberField), map[string]*templateField{"fieldRef": fieldRef, "resourceFieldRef": resourceRef}), false)})
	volume := objectField(fields("name", textField), map[string]*templateField{
		"configMap": configVolume, "secret": configVolume, "downwardAPI": downward,
		"persistentVolumeClaim": objectField(fields("claimName", textField), fields("readOnly", boolField)),
		"emptyDir":              objectField(fields("medium sizeLimit", textField)), "hostPath": objectField(fields("path type", textField)),
		"nfs":       objectField(fields("server path", textField), fields("readOnly", boolField)),
		"csi":       objectField(fields("driver fsType", textField), fields("readOnly", boolField), map[string]*templateField{"nodePublishSecretRef": reference}),
		"projected": objectField(fields("defaultMode", numberField), map[string]*templateField{"sources": arrayField(objectField(map[string]*templateField{"secret": configVolume, "configMap": configVolume, "downwardAPI": downward, "serviceAccountToken": objectField(fields("audience path", textField), fields("expirationSeconds", numberField))}), false)}),
	})
	spec := objectField(fields("serviceAccountName serviceAccount nodeName schedulerName priorityClassName restartPolicy dnsPolicy hostname subdomain runtimeClassName preemptionPolicy os", textField), fields("terminationGracePeriodSeconds activeDeadlineSeconds priority", numberField), fields("automountServiceAccountToken hostNetwork hostPID hostIPC shareProcessNamespace setHostnameAsFQDN enableServiceLinks", boolField), map[string]*templateField{
		"containers": arrayField(container, true), "initContainers": arrayField(container, true), "volumes": arrayField(volume, true), "securityContext": security, "nodeSelector": stringMap(),
		"imagePullSecrets": arrayField(reference, false), "readinessGates": arrayField(objectField(fields("conditionType", textField)), false),
		"tolerations":               arrayField(objectField(fields("key operator value effect", textField), fields("tolerationSeconds", numberField)), false),
		"affinity":                  objectField(map[string]*templateField{"podAffinity": podAffinity, "podAntiAffinity": podAffinity, "nodeAffinity": objectField(map[string]*templateField{"requiredDuringSchedulingIgnoredDuringExecution": objectField(map[string]*templateField{"nodeSelectorTerms": arrayField(nodeTerm, false)}), "preferredDuringSchedulingIgnoredDuringExecution": arrayField(objectField(fields("weight", numberField), map[string]*templateField{"preference": nodeTerm}), false)})}),
		"topologySpreadConstraints": arrayField(objectField(fields("topologyKey whenUnsatisfiable nodeAffinityPolicy nodeTaintsPolicy", textField), fields("maxSkew minDomains", numberField), map[string]*templateField{"labelSelector": selector, "matchLabelKeys": arrayField(textField, false)}), false),
		"schedulingGates":           arrayField(objectField(fields("name", textField)), false),
	})
	spec.fields["os"] = objectField(fields("name", textField))
	return objectField(map[string]*templateField{"metadata": objectField(map[string]*templateField{"annotations": nil, "labels": objectField(map[string]*templateField{"pod-template-hash": {ignore: true}})}), "spec": spec})
}

// ProjectTemplate returns flat schema paths so container reorderings do not
// masquerade as changes and image changes always identify their container.
func ProjectTemplate(raw json.RawMessage) (map[string]any, []string, error) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, nil, errors.New("invalid pod template JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, errors.New("invalid pod template object")
	}
	spec, ok := object["spec"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("pod template spec is required")
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) == 0 {
		return nil, nil, errors.New("pod template containers are required")
	}
	result := map[string]any{}
	excluded := map[string]bool{}
	if err := projectTemplateField(value, templateSchema, "", result, excluded); err != nil {
		return nil, nil, err
	}
	paths := make([]string, 0, len(excluded))
	for path := range excluded {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return result, paths, nil
}

func projectTemplateField(value any, schema *templateField, path string, out map[string]any, excluded map[string]bool) error {
	invalid := func() error { return errors.New("invalid pod template field type or identity") }
	if schema.ignore || value == nil {
		return nil
	}
	if schema.scalar != "" {
		valid := false
		switch value.(type) {
		case string:
			valid = schema.scalar == "string" || schema.scalar == "port"
		case float64:
			valid = schema.scalar == "number" || schema.scalar == "port"
		case bool:
			valid = schema.scalar == "bool"
		}
		if !valid {
			return invalid()
		}
		if text, ok := value.(string); ok {
			value = safety.NewRedactor().Redact(text)
		}
		out[path] = value
		return nil
	}
	if schema.item != nil && !schema.dynamic {
		values, ok := value.([]any)
		if !ok {
			return invalid()
		}
		if schema.environment {
			return projectEnvironment(values, schema.item, path, out, excluded)
		}
		used := map[string]bool{}
		for i, item := range values {
			key := fmt.Sprint(i)
			if schema.named {
				obj, ok := item.(map[string]any)
				if !ok {
					return invalid()
				}
				name, ok := obj["name"].(string)
				if !ok || !templateItemName.MatchString(name) || used[name] {
					return invalid()
				}
				used[name] = true
				key = name
			}
			if err := projectTemplateField(item, schema.item, path+"["+key+"]", out, excluded); err != nil {
				return err
			}
		}
		return nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return invalid()
	}
	for key, item := range obj {
		child, ok := schema.fields[key]
		if schema.dynamic {
			child = schema.item
			ok = true
		}
		if !ok {
			excluded[path+".*"] = true
			continue
		}
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if schema.dynamic {
			childPath = path + "." + safety.NewRedactor().Redact(key)
		}
		if child == nil {
			excluded[childPath] = true
			continue
		}
		if err := projectTemplateField(item, child, childPath, out, excluded); err != nil {
			return err
		}
	}
	return nil
}

// Environment names use relaxed API syntax. Quoting and occurrence indexes keep
// punctuation and duplicate names from overwriting independent reference fields.
func projectEnvironment(values []any, schema *templateField, path string, out map[string]any, excluded map[string]bool) error {
	names := make([]string, len(values))
	counts := map[string]int{}
	for i, item := range values {
		obj, ok := item.(map[string]any)
		if !ok {
			return errors.New("invalid environment entry")
		}
		name, ok := obj["name"].(string)
		if !ok || name == "" {
			return errors.New("invalid environment identity")
		}
		for _, char := range name {
			if char < 32 || char > 126 || char == '=' {
				return errors.New("invalid environment identity")
			}
		}
		names[i] = name
		counts[name]++
	}
	occurrences := map[string]int{}
	for i, item := range values {
		name := names[i]
		if safety.NewRedactor().Redact(name) != name {
			excluded[path+"[*] (redacted environment identity)"] = true
			continue
		}
		itemPath := path + "[" + strconv.Quote(name) + "]"
		if counts[name] > 1 {
			excluded[path+"[*] (duplicate names matched by occurrence order)"] = true
			itemPath += "[" + strconv.Itoa(occurrences[name]) + "]"
			occurrences[name]++
		}
		if err := projectTemplateField(item, schema, itemPath, out, excluded); err != nil {
			return err
		}
	}
	return nil
}

func DiffTemplates(before, after map[string]any) []FieldChange {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	paths := make([]string, 0, len(keys))
	for k := range keys {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	changes := []FieldChange{}
	for _, path := range paths {
		if !reflect.DeepEqual(before[path], after[path]) {
			changes = append(changes, FieldChange{Path: path, Before: before[path], After: after[path]})
		}
	}
	return changes
}
