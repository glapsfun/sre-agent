package guard

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

type stringSet map[string]bool

func set(values ...string) stringSet {
	result := stringSet{}
	for _, value := range values {
		result[value] = true
	}
	return result
}

var investigators = set("sre-change-historian", "sre-eks-investigator", "sre-gke-investigator", "sre-k8s-investigator", "sre-logs-investigator", "sre-metrics-analyst", "sre-trace-analyst")
var harmlessRedirects = set("/dev/null", "/dev/stderr", "/dev/stdout")
var dangerousEnv = set("BASH_ENV", "ENV", "GIT_SSH_COMMAND", "IFS", "PATH", "PYTHONPATH", "PYTHONSTARTUP")
var controlWords = set("!", "do", "elif", "else", "if", "then", "time", "until", "while", "{")
var skippedWords = set("]]", "case", "done", "esac", "fi", "for", "select", "}")
var skillScripts = set("crossplane-status-check.sh", "sre-aws-discovery.sh", "sre-env-discovery.sh", "sre-evidence.sh", "sre-gcloud-discovery.sh", "sre-gh-discovery.sh", "sre-obs-discovery.sh", "sre-snapshot.sh")
var readers = set("[", "[[", "awk", "base64", "basename", "bc", "break", "cat", "cd", "column", "comm", "continue", "cut", "date", "declare", "df", "diff", "dig", "dirname", "du", "echo", "egrep", "exit", "export", "expr", "false", "fgrep", "find", "fold", "getent", "grep", "head", "hexdump", "host", "hostname", "id", "join", "jq", "kill", "local", "ls", "lsof", "netstat", "nl", "nslookup", "od", "paste", "pgrep", "ping", "printf", "ps", "pwd", "read", "return", "rev", "rg", "sed", "seq", "set", "sha256sum", "shasum", "sleep", "sort", "ss", "stat", "tac", "tail", "test", "tr", "traceroute", "true", "type", "uname", "uniq", "unset", "wait", "wc", "which", "whoami")
var printsEnvironmentWithoutArgs = set("declare", "export", "set")
var shells = set("bash", "dash", "ksh", "sh", "zsh")
var kubectlNames = set("kubecolor", "kubectl", "oc")
var kubectlReadVerbs = set("api-resources", "api-versions", "describe", "events", "explain", "logs", "port-forward", "top", "version")
var kubectlIdentityFlags = set("--as", "--as-group", "--as-uid", "--password", "--token", "--username")
var valueFlags = set("--account", "--ca-bundle", "--chunk-size", "--cli-connect-timeout", "--cli-read-timeout", "--cluster", "--color", "--container", "--context", "--endpoint-url", "--field-selector", "--filter", "--format", "--kube-context", "--kubeconfig", "--limit", "--namespace", "--output", "--profile", "--project", "--query", "--raw", "--region", "--repo", "--request-timeout", "--selector", "--server", "--since", "--since-time", "--sort-by", "--tail", "--template", "--timeout", "--user", "--v", "-R", "-c", "-l", "-n", "-o", "-s", "-v")
var execLookups = set("dig", "getent", "host", "nslookup")
var execReadableFiles = set("/etc/hosts", "/etc/resolv.conf")
var gitValueFlags = set("--git-dir", "--work-tree", "-C")
var gitRead = set("blame", "branch", "cat-file", "config", "describe", "diff", "for-each-ref", "grep", "log", "ls-files", "ls-tree", "merge-base", "name-rev", "reflog", "remote", "rev-list", "rev-parse", "shortlog", "show", "status", "tag")
var curlBodyFlags = set("--data", "--data-ascii", "--data-binary", "--data-raw", "--data-urlencode", "--json")
var curlWriteFlags = set("--config", "--cookie-jar", "--create-dirs", "--dump-header", "--form", "--form-string", "--libcurl", "--output-dir", "--remote-name", "--remote-name-all", "--stderr", "--trace", "--trace-ascii", "--upload-file")
var curlLongValueFlags = set("--cacert", "--cert", "--connect-timeout", "--cookie", "--header", "--key", "--max-time", "--proxy", "--range", "--referer", "--resolve", "--retry", "--user", "--user-agent", "--write-out")
var curlShortValueFlags = set("A", "C", "E", "H", "Y", "b", "e", "m", "r", "u", "w", "x", "y", "z")
var curlShortWriteFlags = set("D", "F", "K", "O", "T", "c")
var credentialPath = regexp.MustCompile("\\.kube(/|\\n?$)|\\.aws(/|\\n?$)|\\.config/gcloud|\\.azure(/|\\n?$)|\\.ssh(/|\\n?$)|\\.netrc|\\.pgpass|\\.docker/config|\\.git-credentials|application_default_credentials|/var/run/secrets/|/proc/[^/]+/environ")
var assignment = regexp.MustCompile("^([A-Za-z_][A-Za-z0-9_]*)\\+?=")
var sedRunsOrWrites = regexp.MustCompile("(^|[;{}\\p{Z}\\t\\n\\r\\f\\v\\x{001c}-\\x{001f}/0-9$,])[ew][\\p{Z}\\t\\n\\r\\f\\v\\x{001c}-\\x{001f}]+[^\\p{Z}\\t\\n\\r\\f\\v\\x{001c}-\\x{001f}]|/[gpIi0-9]*[ew]([\\p{Z}\\t\\n\\r\\f\\v\\x{001c}-\\x{001f}]|;|}|$)")
var awsReadOperation = regexp.MustCompile("^(describe|list|get|lookup|search|filter|batch-get|head)-")
var awsSecretOperation = regexp.MustCompile("secret-value|password|session-token|federation-token|authorization-token|get-token|credential")
var gcloudReadVerb = regexp.MustCompile("^(list.*|describe.*|read|tail|search.*|analyze.*|get-.*|info|version)\\n?$")
var gcloudWriteVerb = regexp.MustCompile("^(create|delete|update|patch|set|unset|resize|start|stop|restart|reset|import|export|deploy|apply|ssh|scp|access|login|revoke|activate.*|enable|disable|cancel|attach|detach|move|clone|restore|upgrade|rollback|get-credentials|print-.*|set-.*|add-.*|remove-.*)\\n?$")
var searchReadPath = regexp.MustCompile("/_(search|count|msearch|cat|mapping|field_caps|cluster/health)([^\\p{L}\\p{N}_]|$)")
var wrappers = map[string]stringSet{
	"command": set(),
	"builtin": set(),
	"exec":    set(),
	"nohup":   set(),
	"nice":    set("--adjustment", "-n"),
	"stdbuf":  set("-e", "-i", "-o"),
	"timeout": set("--kill-after", "--signal", "-k", "-s"),
	"watch":   set("--interval", "-n"),
	"xargs":   set("--delimiter", "--max-args", "--max-procs", "--replace", "-E", "-I", "-L", "-P", "-a", "-d", "-n", "-s"),
	"env":     set("--chdir", "--unset", "-C", "-u"),
}
var toolTables = map[string]map[string]stringSet{
	"helm": {
		"list":    nil,
		"ls":      nil,
		"status":  nil,
		"history": nil,
		"hist":    nil,
		"version": nil,
		"show":    nil,
		"search":  nil,
		"get":     set("hooks", "metadata", "notes"),
	},
	"flux": {
		"get":     nil,
		"tree":    nil,
		"trace":   nil,
		"logs":    nil,
		"stats":   nil,
		"version": nil,
		"check":   nil,
		"diff":    nil,
		"events":  nil,
	},
	"argocd": {
		"version": nil,
		"context": nil,
		"app":     set("diff", "get", "history", "list", "logs", "resources"),
		"proj":    set("get", "list"),
		"repo":    set("get", "list"),
		"cluster": set("get", "list"),
		"account": set("can-i"),
	},
	"istioctl": {
		"proxy-status": nil,
		"ps":           nil,
		"proxy-config": nil,
		"pc":           nil,
		"analyze":      nil,
		"version":      nil,
		"describe":     nil,
	},
	"linkerd": {
		"check":       nil,
		"version":     nil,
		"diagnostics": nil,
		"viz":         set("check", "edges", "routes", "stat", "tap", "top"),
	},
	"crossplane": {
		"version":  nil,
		"resource": set("trace"),
		"beta":     set("trace"),
	},
	"k6": {
		"version": nil,
		"inspect": nil,
	},
	"eksctl": {
		"get":     nil,
		"version": nil,
	},
	"gh": {
		"pr":       set("checks", "diff", "list", "status", "view"),
		"issue":    set("list", "status", "view"),
		"run":      set("list", "view"),
		"repo":     set("list", "view"),
		"release":  set("list", "view"),
		"workflow": set("list", "view"),
		"label":    set("list"),
		"search":   nil,
		"auth":     set("status"),
		"version":  nil,
	},
	"glab": {
		"mr":      set("diff", "list", "view"),
		"issue":   set("list", "view"),
		"ci":      set("get", "list", "status", "trace", "view"),
		"release": set("list", "view"),
		"repo":    set("view"),
		"auth":    set("status"),
		"version": nil,
	},
}

func baseName(name string) string { return name[strings.LastIndex(name, "/")+1:] }
func assignmentName(word string) string {
	match := assignment.FindStringSubmatch(word)
	if match == nil {
		return ""
	}
	return match[1]
}
func dangerousName(name string) bool {
	return dangerousEnv[name] || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
}
func first(words []string) string {
	if len(words) == 0 {
		return ""
	}
	return words[0]
}
func has(words []string, value string) bool {
	for _, word := range words {
		if word == value {
			return true
		}
	}
	return false
}
func flagName(arg string) string { name, _, _ := strings.Cut(arg, "="); return name }

func checkWords(words []string) string {
	for len(words) > 0 {
		name := assignmentName(words[0])
		if controlWords[words[0]] {
			words = words[1:]
		} else if name != "" {
			if dangerousName(name) {
				return "it sets " + name
			}
			words = words[1:]
		} else {
			break
		}
	}
	if len(words) == 0 || skippedWords[words[0]] {
		return ""
	}
	name, args := words[0], words[1:]
	if strings.Contains(name, dynamic) {
		return "its command name comes from a variable"
	}
	base := baseName(name)
	if skillScripts[base] {
		return ""
	}
	if _, ok := wrappers[base]; ok {
		return checkWrapper(base, args)
	}
	if shells[base] {
		return checkShell(base, args)
	}
	if kubectlNames[base] {
		return checkKubectl(base, args)
	}
	if _, ok := toolTables[base]; ok {
		return checkTable(base, args)
	}
	if readers[base] {
		return checkReader(base, args)
	}
	switch base {
	case "aws":
		return checkAWS(args)
	case "gcloud":
		return checkGcloud(args)
	case "git":
		return checkGit(args)
	case "curl":
		return checkCurl(args)
	}
	return base + " isn't on the read-only list"
}

func checkWrapper(base string, args []string) string {
	if base == "command" && (first(args) == "-v" || first(args) == "-V") {
		return ""
	}
	rest := args
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		flag := rest[0]
		rest = rest[1:]
		if flag == "--" {
			break
		}
		if base == "env" && (flag == "-S" || flag == "--split-string") || strings.HasPrefix(flag, "--split-string=") {
			return "env -S hides the command in a string"
		}
		if wrappers[base][flag] && len(rest) > 0 {
			rest = rest[1:]
		}
	}
	if base == "timeout" && len(rest) > 0 {
		rest = rest[1:]
	}
	if base == "env" {
		for len(rest) > 0 && assignmentName(rest[0]) != "" {
			name := assignmentName(rest[0])
			rest = rest[1:]
			if dangerousName(name) {
				return "it sets " + name
			}
		}
		if len(rest) == 0 {
			return "env with no command prints the environment"
		}
	}
	return checkWords(rest)
}

func checkShell(base string, args []string) string {
	index := 0
	for index < len(args) {
		arg := args[index]
		if arg == "--" {
			index++
			break
		}
		if arg == "-o" || arg == "+o" || arg == "-O" || arg == "+O" {
			index += 2
			continue
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c") {
			if index+1 >= len(args) {
				return base + " -c has no command"
			}
			script := args[index+1]
			if strings.Contains(script, dynamic) {
				return "the command given to " + base + " -c isn't literal"
			}
			return checkCommandLine(script)
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
			break
		}
		index++
	}
	if index >= len(args) {
		return base + " would run whatever it reads from its input"
	}
	if skillScripts[baseName(args[index])] {
		return ""
	}
	return base + " would run a script the guard can't read"
}

func positionals(args []string, valueFlags stringSet) []string {
	var found []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			if !strings.Contains(arg, "=") && valueFlags[arg] {
				index++
			}
		} else {
			found = append(found, arg)
		}
	}
	return found
}

func flagValue(args []string, names ...string) (string, bool) {
	value, found := "", false
	for index, arg := range args {
		for _, name := range names {
			if arg == name && index+1 < len(args) {
				value, found = args[index+1], true
			} else if strings.HasPrefix(arg, name+"=") {
				value, found = arg[len(name)+1:], true
			}
		}
	}
	return value, found
}

func checkKubectl(base string, args []string) string {
	for _, arg := range args {
		if kubectlIdentityFlags[flagName(arg)] {
			return "it changes who " + base + " acts as"
		}
	}
	words := positionals(args, valueFlags)
	if len(words) == 0 {
		return ""
	}
	verb, rest := words[0], words[1:]
	second := first(rest)
	if strings.Contains(verb, dynamic) {
		return "its verb comes from a variable"
	}
	if kubectlReadVerbs[verb] {
		return ""
	}
	switch verb {
	case "get":
		return checkKubectlGet(args, rest)
	case "exec":
		return checkKubectlExec(args)
	case "rollout":
		if second == "status" || second == "history" {
			return ""
		}
	case "auth":
		if second == "can-i" || second == "whoami" {
			return ""
		}
	case "cluster-info":
		if second != "dump" {
			return ""
		}
	case "config":
		if second == "current-context" || second == "get-contexts" || second == "get-clusters" {
			return ""
		}
		if second == "view" {
			safe := true
			for _, arg := range args {
				if flagName(arg) == "--raw" || flagName(arg) == "--flatten" {
					safe = false
				}
			}
			if safe {
				return ""
			}
		}
	}
	return strings.TrimRightFunc(base+" "+verb+" "+second, pythonSpace) + " isn't a read-only command"
}

func outputFormat(args []string) (string, bool) {
	value, found := "", false
	for index, arg := range args {
		if (arg == "-o" || arg == "--output") && index+1 < len(args) {
			value, found = args[index+1], true
		} else if strings.HasPrefix(arg, "--output=") {
			value, found = strings.TrimPrefix(arg, "--output="), true
		} else if strings.HasPrefix(arg, "-o") && !strings.HasPrefix(arg, "--") {
			value, found = strings.TrimLeft(arg[2:], "="), true
		} else if flagName(arg) == "--template" || flagName(arg) == "--go-template" {
			value, found = "template", true
		}
	}
	return value, found
}

var secretRawPath = regexp.MustCompile(`/secrets(/|\?|\n?$)`)

// PathUnescape rejects malformed escapes; Python's unquote leaves those bytes intact.
func unquotePath(value string) string {
	var out strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '%' && index+2 < len(value) {
			if decoded, err := url.PathUnescape(value[index : index+3]); err == nil {
				out.WriteString(decoded)
				index += 2
				continue
			}
		}
		out.WriteByte(value[index])
	}
	return out.String()
}

func checkKubectlGet(args, resources []string) string {
	if raw, found := flagValue(args, "--raw"); found {
		if strings.Contains(raw, dynamic) {
			return "the --raw path comes from a variable"
		}
		if secretRawPath.MatchString(unquotePath(raw)) {
			return "it reads Secret values"
		}
		return ""
	}
	output, found := outputFormat(args)
	if !found || output == "name" || output == "wide" {
		return ""
	}
	if len(resources) == 0 {
		return "the guard can't tell what this reads"
	}
	if strings.Contains(resources[0], dynamic) {
		return "the resource type comes from a variable, and the output format would show Secret values"
	}
	for _, resource := range resources {
		for _, part := range strings.Split(resource, ",") {
			kind := strings.ToLower(strings.SplitN(strings.SplitN(part, "/", 2)[0], ".", 2)[0])
			if kind == "secret" || kind == "secrets" {
				return "it reads Secret values"
			}
		}
	}
	return ""
}

func checkKubectlExec(args []string) string {
	index := -1
	for i, arg := range args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return "kubectl exec needs -- before the command, so the guard can read it"
	}
	command := args[index+1:]
	if len(command) == 0 {
		return "kubectl exec has no command"
	}
	name := baseName(command[0])
	if execLookups[name] {
		return ""
	}
	if name == "cat" && len(command) > 1 {
		safe := true
		for _, path := range command[1:] {
			if !execReadableFiles[path] {
				safe = false
			}
		}
		if safe {
			return ""
		}
	}
	return "kubectl exec is limited to DNS lookups and resolver files, not " + name
}

func checkTable(base string, args []string) string {
	words := positionals(args, valueFlags)
	if len(words) == 0 {
		return ""
	}
	firstWord := words[0]
	if (base == "gh" || base == "glab") && firstWord == "api" {
		return checkForgeAPI(args)
	}
	allowed, exists := toolTables[base][firstWord]
	if !exists {
		return base + " " + firstWord + " isn't a read-only command"
	}
	if allowed == nil {
		return ""
	}
	second := first(words[1:])
	if allowed[second] {
		return ""
	}
	return strings.TrimRightFunc(base+" "+firstWord+" "+second, pythonSpace) + " isn't a read-only command"
}

func checkForgeAPI(args []string) string {
	for index, arg := range args {
		name := flagName(arg)
		method, found := "", false
		if name == "-X" || name == "--method" {
			found = true
			if _, value, inline := strings.Cut(arg, "="); inline {
				method = value
			} else if index+1 < len(args) {
				method = args[index+1]
			}
		} else if strings.HasPrefix(arg, "-X") {
			method, found = arg[2:], true
		}
		if found && strings.ToUpper(method) != "GET" {
			return "the API call isn't a GET"
		}
		if name == "-f" || name == "-F" || name == "--field" || name == "--raw-field" || name == "--input" {
			return "sending fields turns the API call into a POST"
		}
	}
	return ""
}

func checkAWS(args []string) string {
	words := positionals(args, valueFlags)
	if len(words) < 2 {
		return "the guard can't tell what this aws call does"
	}
	service, operation := words[0], words[1]
	if has(args, "--with-decryption") || awsSecretOperation.MatchString(operation) {
		return "it reads a secret or a credential"
	}
	if service == "s3" && operation == "ls" || service == "logs" && (operation == "tail" || operation == "start-query" || operation == "stop-query") || service == "iam" && operation == "simulate-principal-policy" || awsReadOperation.MatchString(operation) {
		return ""
	}
	return "aws " + service + " " + operation + " isn't a read-only call"
}

func checkGcloud(args []string) string {
	var words []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			words = append(words, arg)
		}
	}
	for _, word := range words {
		if gcloudWriteVerb.MatchString(word) {
			return "gcloud " + word + " isn't a read-only call"
		}
	}
	for _, word := range words {
		if gcloudReadVerb.MatchString(word) {
			return ""
		}
	}
	return "the guard can't find a read-only verb in this gcloud call"
}

func checkGit(args []string) string {
	rest := args
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		flag := rest[0]
		rest = rest[1:]
		if flag == "-c" || strings.HasPrefix(flag, "--exec-path") || strings.HasPrefix(flag, "--config-env") {
			return "git -c can run commands"
		}
		if gitValueFlags[flag] && len(rest) > 0 {
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return ""
	}
	subcommand, tail := rest[0], rest[1:]
	if !gitRead[subcommand] {
		return "git " + subcommand + " isn't a read-only command"
	}
	for _, arg := range tail {
		name := flagName(arg)
		if name == "--output" || name == "--ext-diff" || name == "--open-files-in-pager" || strings.HasPrefix(arg, "-O") {
			return "it writes a file or runs an external program"
		}
	}
	switch subcommand {
	case "remote":
		if len(tail) > 0 && !set("-v", "--verbose", "show", "get-url")[tail[0]] {
			return "it changes a remote"
		}
	case "reflog":
		if first(tail) == "expire" || first(tail) == "delete" {
			return "it changes the reflog"
		}
	case "config":
		safe := false
		for _, arg := range tail {
			if arg == "-l" || arg == "--list" || strings.HasPrefix(arg, "--get") {
				safe = true
			}
		}
		if !safe {
			return "it changes git config"
		}
	case "branch":
		for _, arg := range tail {
			if !(strings.HasPrefix(arg, "-") && !set("-d", "-D", "-m", "-M", "-c", "-C", "-f", "--delete", "--move", "--copy", "--force")[arg] || has(tail, "--list") || has(tail, "--contains")) {
				return "it changes a branch"
			}
		}
	case "tag":
		safe := false
		for _, arg := range tail {
			if set("-l", "--list", "--contains", "--points-at")[arg] || strings.HasPrefix(arg, "--list") {
				safe = true
			}
		}
		if !safe {
			return "it changes a tag"
		}
	}
	return ""
}

func checkCurl(args []string) string {
	forcedGet, hasBody, hasMethod := false, false, false
	method := ""
	var urls []string
	index := 0
	for index < len(args) {
		arg := args[index]
		index++
		if strings.HasPrefix(arg, "--") {
			name, inline, hasInline := strings.Cut(arg, "=")
			value := func() string {
				if hasInline {
					return inline
				}
				index++
				if index <= len(args) {
					return args[index-1]
				}
				return ""
			}
			switch {
			case name == "--get":
				forcedGet = true
			case name == "--request":
				method, hasMethod = value(), true
			case name == "--url":
				urls = append(urls, value())
			case curlBodyFlags[name]:
				hasBody = true
				value()
			case name == "--output":
				if !harmlessRedirects[value()] {
					return "curl would write a file"
				}
			case curlWriteFlags[name]:
				return "curl " + name + " writes or sends a file"
			case curlLongValueFlags[name]:
				value()
			}
		} else if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			chars := []rune(arg)
			for position := 1; position < len(chars); position++ {
				flag := string(chars[position])
				attached := string(chars[position+1:])
				if flag == "G" {
					forcedGet = true
				} else if curlShortWriteFlags[flag] {
					return "curl -" + flag + " writes or sends a file"
				} else if strings.Contains("Xdo", flag) || curlShortValueFlags[flag] {
					taken := attached
					if attached == "" {
						taken = ""
						if index < len(args) {
							taken = args[index]
						}
						index++
					}
					if flag == "X" {
						method, hasMethod = taken, true
					} else if flag == "d" {
						hasBody = true
					} else if flag == "o" && !harmlessRedirects[taken] {
						return "curl would write a file"
					}
					break
				}
			}
		} else {
			urls = append(urls, arg)
		}
	}
	if hasMethod && !set("GET", "HEAD", "POST")[strings.ToUpper(method)] {
		return "curl " + method + " isn't a read"
	}
	sends := hasBody && !forcedGet || strings.ToUpper(method) == "POST"
	if sends {
		for _, address := range urls {
			if searchReadPath.MatchString(address) {
				return ""
			}
		}
		return "curl would send data, and this isn't a search endpoint"
	}
	return ""
}

func checkReader(base string, args []string) string {
	if printsEnvironmentWithoutArgs[base] && len(args) == 0 {
		return base + " with no arguments prints the environment"
	}
	for _, arg := range args {
		if credentialPath.MatchString(arg) {
			return "it reads a credential file"
		}
	}
	if printsEnvironmentWithoutArgs[base] {
		for _, arg := range args {
			if name := assignmentName(arg); name != "" && dangerousName(name) {
				return "it sets " + name
			}
		}
	}
	for _, arg := range args {
		switch base {
		case "rg":
			if flagName(arg) == "--pre" {
				return "rg --pre runs a command"
			}
		case "sed":
			if strings.HasPrefix(arg, "-i") || strings.HasPrefix(arg, "--in-place") || strings.HasPrefix(arg, "-f") || sedRunsOrWrites.MatchString(arg) {
				return "sed would edit a file or run a command"
			}
		case "sort":
			if strings.HasPrefix(arg, "-o") || strings.HasPrefix(arg, "--output") || strings.HasPrefix(arg, "--compress-program") {
				return "sort would write a file"
			}
		case "find":
			if set("-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls")[arg] {
				return "find would change something or run a command"
			}
		case "awk":
			if strings.Contains(arg, "system(") || strings.Contains(arg, "getline") || strings.Contains(arg, "|") && strings.Contains(arg, `"`) || strings.Contains(arg, "print >") || strings.Contains(arg, "printf >") {
				return "awk would run a command or write a file"
			}
		}
	}
	return ""
}

func pythonSpace(char rune) bool { return unicode.IsSpace(char) || char >= 0x1c && char <= 0x1f }
