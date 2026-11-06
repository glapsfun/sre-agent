#!/usr/bin/env python3
"""Read-only guard for the sre-* investigator subagents.

Runs before their tool calls. It follows enough shell to find every command in a
line, including pipes, loops, and $(...), and allows only commands known to read.
Anything it doesn't recognize is blocked.

This stops mistakes and simple prompt injection. It's not a sandbox: a read-only
cluster and cloud identity is the hard limit. It can't see inside a script it
allows, and a GET request can still carry data out.
"""

import json
import os
import re
import sys
from urllib.parse import unquote

INVESTIGATORS = {
    "sre-k8s-investigator",
    "sre-metrics-analyst",
    "sre-logs-investigator",
    "sre-change-historian",
    "sre-trace-analyst",
    "sre-eks-investigator",
    "sre-gke-investigator",
}

# Stands in for anything only the shell can resolve, such as $x or $(cmd).
DYNAMIC = "\0"

CREDENTIAL_PATH = re.compile(
    r"\.kube(/|$)|\.aws(/|$)|\.config/gcloud|\.azure(/|$)|\.ssh(/|$)|\.netrc|\.pgpass|\.docker/config"
    r"|\.git-credentials|application_default_credentials|/var/run/secrets/|/proc/[^/]+/environ"
)

HARMLESS_REDIRECTS = {"/dev/null", "/dev/stdout", "/dev/stderr"}
DANGEROUS_ENV = {"PATH", "IFS", "ENV", "BASH_ENV", "PYTHONPATH", "PYTHONSTARTUP", "GIT_SSH_COMMAND"}
ASSIGNMENT = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)\+?=")

CONTROL_WORDS = {"if", "then", "elif", "else", "do", "while", "until", "!", "{", "time"}
SKIPPED_WORDS = {"for", "select", "case", "fi", "done", "esac", "}", "]]"}

SKILL_SCRIPTS = {
    "sre-env-discovery.sh",
    "sre-obs-discovery.sh",
    "sre-evidence.sh",
    "sre-snapshot.sh",
    "sre-gh-discovery.sh",
    "sre-aws-discovery.sh",
    "sre-gcloud-discovery.sh",
    "crossplane-status-check.sh",
}

READERS = {
    "[", "[[", "awk", "base64", "basename", "bc", "break", "cat", "cd", "column", "comm", "continue",
    "cut", "date", "declare", "df", "diff", "dig", "dirname", "du", "echo", "egrep", "exit", "export",
    "expr", "false", "fgrep", "find", "fold", "getent", "grep", "head", "hexdump", "host",
    "hostname", "id", "join", "jq", "kill", "local", "ls", "lsof", "netstat", "nl", "nslookup", "od",
    "paste", "pgrep", "ping", "printf", "ps", "pwd", "read", "return", "rev", "rg", "sed", "seq", "set",
    "sha256sum", "shasum", "sleep", "sort", "ss", "stat", "tac", "tail", "test", "tr", "traceroute",
    "true", "type", "uname", "uniq", "unset", "wait", "wc", "which", "whoami",
}
PRINTS_ENVIRONMENT_WITHOUT_ARGS = {"set", "export", "declare"}

SHELLS = {"bash", "sh", "zsh", "dash", "ksh"}

# GNU sed runs a command with e, and writes a file with w.
SED_RUNS_OR_WRITES = re.compile(r"(^|[;{}\s/0-9$,])[ew]\s+\S|/[gpIi0-9]*[ew](\s|;|}|$)")

# Flags that take a value, for wrappers around another command.
WRAPPERS = {
    "command": set(),
    "builtin": set(),
    "exec": set(),
    "nohup": set(),
    "nice": {"-n", "--adjustment"},
    "stdbuf": {"-i", "-o", "-e"},
    "timeout": {"-k", "-s", "--kill-after", "--signal"},
    "watch": {"-n", "--interval"},
    "xargs": {"-I", "-n", "-P", "-L", "-d", "-E", "-s", "-a", "--replace", "--max-args", "--max-procs", "--delimiter"},
    "env": {"-u", "--unset", "-C", "--chdir"},
}

KUBECTL_NAMES = {"kubectl", "oc", "kubecolor"}
KUBECTL_READ_VERBS = {
    "describe", "logs", "top", "events", "explain", "version", "api-resources", "api-versions", "port-forward",
}
KUBECTL_IDENTITY_FLAGS = {"--as", "--as-group", "--as-uid", "--token", "--username", "--password"}
VALUE_FLAGS = {
    "-n", "--namespace", "--context", "--kube-context", "--kubeconfig", "--cluster", "--user", "-s", "--server",
    "--request-timeout", "--timeout", "-l", "--selector", "-o", "--output", "--field-selector", "--sort-by",
    "--chunk-size", "--template", "--raw", "-c", "--container", "--tail", "--since", "--since-time", "-v", "--v",
    "-R", "--repo", "--profile", "--region", "--query", "--endpoint-url", "--color", "--project", "--account",
    "--format", "--filter", "--limit", "--cli-read-timeout", "--cli-connect-timeout", "--ca-bundle",
}
SAFE_SECRET_OUTPUTS = {None, "name", "wide"}
EXEC_LOOKUPS = {"nslookup", "getent", "dig", "host"}
EXEC_READABLE_FILES = {"/etc/resolv.conf", "/etc/hosts"}

# First word, then the second word when it matters. None means any second word.
TOOL_TABLES = {
    "helm": {
        "list": None, "ls": None, "status": None, "history": None, "hist": None, "version": None, "show": None,
        "search": None, "get": {"metadata", "hooks", "notes"},
    },
    "flux": {
        "get": None, "tree": None, "trace": None, "logs": None, "stats": None, "version": None, "check": None,
        "diff": None, "events": None,
    },
    "argocd": {
        "version": None, "context": None, "app": {"get", "list", "history", "diff", "resources", "logs"},
        "proj": {"list", "get"}, "repo": {"list", "get"}, "cluster": {"list", "get"}, "account": {"can-i"},
    },
    "istioctl": {
        "proxy-status": None, "ps": None, "proxy-config": None, "pc": None, "analyze": None, "version": None,
        "describe": None,
    },
    "linkerd": {
        "check": None, "version": None, "diagnostics": None, "viz": {"stat", "top", "tap", "routes", "edges", "check"},
    },
    "crossplane": {"version": None, "resource": {"trace"}, "beta": {"trace"}},
    "k6": {"version": None, "inspect": None},
    "eksctl": {"get": None, "version": None},
    "gh": {
        "pr": {"list", "view", "diff", "checks", "status"}, "issue": {"list", "view", "status"},
        "run": {"list", "view"}, "repo": {"view", "list"}, "release": {"list", "view"},
        "workflow": {"list", "view"}, "label": {"list"}, "search": None, "auth": {"status"}, "version": None,
    },
    "glab": {
        "mr": {"list", "view", "diff"}, "issue": {"list", "view"}, "ci": {"list", "view", "status", "trace", "get"},
        "release": {"list", "view"}, "repo": {"view"}, "auth": {"status"}, "version": None,
    },
}

AWS_READ_OPERATION = re.compile(r"^(describe|list|get|lookup|search|filter|batch-get|head)-")
AWS_SECRET_OPERATION = re.compile(r"secret-value|password|session-token|federation-token|authorization-token|get-token|credential")
AWS_OTHER_READS = {("s3", "ls"), ("logs", "tail"), ("logs", "start-query"), ("logs", "stop-query"), ("iam", "simulate-principal-policy")}

GCLOUD_READ_VERB = re.compile(r"^(list.*|describe.*|read|tail|search.*|analyze.*|get-.*|info|version)$")
GCLOUD_WRITE_VERB = re.compile(
    r"^(create|delete|update|patch|set|unset|resize|start|stop|restart|reset|import|export|deploy|apply|ssh|scp|access"
    r"|login|revoke|activate.*|enable|disable|cancel|attach|detach|move|clone|restore|upgrade|rollback|get-credentials"
    r"|print-.*|set-.*|add-.*|remove-.*)$"
)

GIT_VALUE_FLAGS = {"-C", "--git-dir", "--work-tree"}
GIT_READ = {
    "log", "show", "diff", "status", "blame", "rev-parse", "rev-list", "ls-files", "ls-tree", "grep", "describe",
    "shortlog", "cat-file", "merge-base", "for-each-ref", "name-rev", "reflog", "remote", "branch", "tag", "config",
}

# Elasticsearch and OpenSearch take queries as a POST body. These paths only read.
SEARCH_READ_PATH = re.compile(r"/_(search|count|msearch|cat|mapping|field_caps|cluster/health)(\b|/|\?|$)")
CURL_BODY_FLAGS = {"--data", "--data-raw", "--data-binary", "--data-ascii", "--data-urlencode", "--json"}
CURL_WRITE_FLAGS = {
    "--remote-name", "--remote-name-all", "--output-dir", "--dump-header", "--cookie-jar", "--trace", "--trace-ascii",
    "--stderr", "--libcurl", "--create-dirs", "--upload-file", "--config", "--form", "--form-string",
}
CURL_LONG_VALUE_FLAGS = {
    "--header", "--max-time", "--connect-timeout", "--user", "--user-agent", "--referer", "--cookie", "--proxy",
    "--write-out", "--retry", "--cacert", "--cert", "--key", "--resolve", "--range",
}
CURL_SHORT_VALUE_FLAGS = set("HmuAebxwrYyzEC")
CURL_SHORT_WRITE_FLAGS = set("OTFKDc")


class Scanner:
    """Splits shell text into simple commands, following $(...) and backticks."""

    def __init__(self, text):
        self.text = text
        self.pos = 0
        self.commands = []
        self.problems = []

    def peek(self, offset=0):
        index = self.pos + offset
        return self.text[index] if index < len(self.text) else ""

    def scan(self, closer=""):
        words = []
        word = None
        redirecting = False
        depth = 0

        def end_word():
            nonlocal word, redirecting
            if word is None:
                return
            if redirecting:
                if word not in HARMLESS_REDIRECTS:
                    self.problems.append("it writes to a file")
                redirecting = False
            else:
                words.append(word)
            word = None

        def end_command():
            nonlocal words
            end_word()
            if words:
                self.commands.append(words)
            words = []

        while self.pos < len(self.text):
            char = self.peek()
            if closer and char == closer and depth == 0:
                self.pos += 1
                end_command()
                return
            if char in " \t":
                end_word()
                self.pos += 1
            elif char == "\\":
                if self.peek(1) != "\n":
                    word = (word or "") + self.peek(1)
                self.pos += 2
            elif char in "\n;|":
                end_command()
                self.pos += 1
            elif char == "&":
                if self.peek(1) == ">":
                    end_word()
                    self.pos += 3 if self.peek(2) == ">" else 2
                    redirecting = True
                else:
                    end_command()
                    self.pos += 1
            elif char in "()":
                if closer:
                    depth += 1 if char == "(" else -1
                end_command()
                self.pos += 1
            elif char == ">":
                if word is not None and word.isdigit():
                    word = None
                end_word()
                redirecting = self.start_redirect()
            elif char == "<":
                end_word()
                if self.peek(1) == "<" and self.peek(2) != "<":
                    self.problems.append("it uses a here-document")
                self.pos += 3 if self.peek(1) == "<" else 1
            elif char == "#" and word is None:
                newline = self.text.find("\n", self.pos)
                self.pos = newline if newline != -1 else len(self.text)
            elif char == "'":
                end = self.text.find("'", self.pos + 1)
                if end == -1:
                    self.problems.append("it has an unterminated quote")
                    self.pos = len(self.text)
                else:
                    word = (word or "") + self.text[self.pos + 1 : end]
                    self.pos = end + 1
            elif char == '"':
                word = (word or "") + self.double_quoted()
            elif char == "$":
                word = (word or "") + self.dollar()
            elif char == "`":
                word = (word or "") + self.backtick()
            else:
                word = (word or "") + char
                self.pos += 1

        if closer:
            self.problems.append("a command substitution never closes")
        end_command()

    def start_redirect(self):
        """Moves past '>' and says whether the next word is a file being written."""
        self.pos += 1
        following = self.peek()
        if following in (">", "|"):
            self.pos += 1
        elif following == "&":
            self.pos += 1
            target = self.peek()
            if target.isdigit() or target == "-":
                self.pos += 1
                return False
        elif following == "(":
            return False
        return True

    def double_quoted(self):
        self.pos += 1
        value = ""
        while self.pos < len(self.text):
            char = self.peek()
            if char == '"':
                self.pos += 1
                return value
            if char == "\\":
                following = self.peek(1)
                if following == "\n":
                    pass
                elif following in '$`"\\':
                    value += following
                else:
                    value += char + following
                self.pos += 2
            elif char == "$":
                value += self.dollar()
            elif char == "`":
                value += self.backtick()
            else:
                value += char
                self.pos += 1
        self.problems.append("it has an unterminated quote")
        return value

    def dollar(self):
        following = self.peek(1)
        if following == "(":
            if self.peek(2) == "(":
                return self.arithmetic()
            self.pos += 2
            self.scan(")")
            return DYNAMIC
        if following == "{":
            end = self.text.find("}", self.pos)
            body = self.text[self.pos : end] if end != -1 else self.text[self.pos :]
            if end == -1 or "$(" in body or "`" in body:
                self.problems.append("it hides a command inside ${...}")
            self.pos = end + 1 if end != -1 else len(self.text)
            return DYNAMIC
        if following == "'":
            end = self.text.find("'", self.pos + 2)
            self.pos = end + 1 if end != -1 else len(self.text)
            return DYNAMIC
        if following == '"':
            self.pos += 1
            return ""
        if following.isalnum() or following == "_":
            self.pos += 1
            while self.peek().isalnum() or self.peek() == "_":
                self.pos += 1
            return DYNAMIC
        if following and following in "@*#?$!-":
            self.pos += 2
            return DYNAMIC
        self.pos += 1
        return "$"

    def arithmetic(self):
        end = self.text.find("))", self.pos)
        body = self.text[self.pos + 3 : end] if end != -1 else self.text[self.pos + 3 :]
        if end == -1 or "$(" in body or "`" in body:
            self.problems.append("it hides a command inside $((...))")
        self.pos = end + 2 if end != -1 else len(self.text)
        return DYNAMIC

    def backtick(self):
        self.pos += 1
        self.scan("`")
        return DYNAMIC


def check_command_line(text):
    scanner = Scanner(text)
    scanner.scan()
    if scanner.problems:
        return scanner.problems[0]
    for words in scanner.commands:
        reason = check_words(words)
        if reason:
            return reason
    return None


def check_words(words):
    words = list(words)
    while words:
        match = ASSIGNMENT.match(words[0])
        if words[0] in CONTROL_WORDS:
            words.pop(0)
        elif match:
            name = match.group(1)
            if name in DANGEROUS_ENV or name.startswith("LD_") or name.startswith("DYLD_"):
                return f"it sets {name}"
            words.pop(0)
        else:
            break
    if not words or words[0] in SKIPPED_WORDS:
        return None

    name, args = words[0], words[1:]
    if DYNAMIC in name:
        return "its command name comes from a variable"
    base = os.path.basename(name)

    if base in SKILL_SCRIPTS:
        return None
    if base in WRAPPERS:
        return check_wrapper(base, args)
    if base in SHELLS:
        return check_shell(base, args)
    if base in KUBECTL_NAMES:
        return check_kubectl(base, args)
    if base in TOOL_TABLES:
        return check_table(base, args)
    if base in READERS:
        return check_reader(base, args)
    special = {"aws": check_aws, "gcloud": check_gcloud, "git": check_git, "curl": check_curl}.get(base)
    if special:
        return special(args)
    return f"{base} isn't on the read-only list"


def check_wrapper(base, args):
    if base == "command" and args and args[0] in ("-v", "-V"):
        return None
    rest = list(args)
    while rest and rest[0].startswith("-"):
        flag = rest.pop(0)
        if flag == "--":
            break
        if base == "env" and flag in ("-S", "--split-string") or flag.startswith("--split-string="):
            return "env -S hides the command in a string"
        if flag in WRAPPERS[base] and rest:
            rest.pop(0)
    if base == "timeout" and rest:
        rest.pop(0)
    if base == "env":
        while rest and ASSIGNMENT.match(rest[0]):
            match = ASSIGNMENT.match(rest.pop(0))
            if match.group(1) in DANGEROUS_ENV or match.group(1).startswith(("LD_", "DYLD_")):
                return f"it sets {match.group(1)}"
        if not rest:
            return "env with no command prints the environment"
    return check_words(rest)


def check_shell(base, args):
    index = 0
    while index < len(args):
        arg = args[index]
        if arg == "--":
            index += 1
            break
        if arg in ("-o", "+o", "-O", "+O"):
            index += 2
            continue
        if arg.startswith("-") and not arg.startswith("--") and "c" in arg:
            if index + 1 >= len(args):
                return f"{base} -c has no command"
            script = args[index + 1]
            if DYNAMIC in script:
                return f"the command given to {base} -c isn't literal"
            return check_command_line(script)
        if not arg.startswith(("-", "+")):
            break
        index += 1
    if index >= len(args):
        return f"{base} would run whatever it reads from its input"
    if os.path.basename(args[index]) in SKILL_SCRIPTS:
        return None
    return f"{base} would run a script the guard can't read"


def positionals(args, value_flags):
    found = []
    index = 0
    while index < len(args):
        arg = args[index]
        if arg == "--":
            break
        if arg.startswith("-") and len(arg) > 1:
            if "=" not in arg and arg in value_flags:
                index += 1
        else:
            found.append(arg)
        index += 1
    return found


def flag_value(args, *names):
    """The value of the last use of a flag, given as '--flag x' or '--flag=x'."""
    value = None
    for index, arg in enumerate(args):
        for name in names:
            if arg == name and index + 1 < len(args):
                value = args[index + 1]
            elif arg.startswith(name + "="):
                value = arg[len(name) + 1 :]
    return value


def check_kubectl(base, args):
    for arg in args:
        if arg.split("=", 1)[0] in KUBECTL_IDENTITY_FLAGS:
            return f"it changes who {base} acts as"
    words = positionals(args, VALUE_FLAGS)
    if not words:
        return None
    verb, rest = words[0], words[1:]
    second = rest[0] if rest else ""
    if DYNAMIC in verb:
        return "its verb comes from a variable"
    if verb in KUBECTL_READ_VERBS:
        return None
    if verb == "get":
        return check_kubectl_get(args, rest)
    if verb == "exec":
        return check_kubectl_exec(args)
    if verb == "rollout" and second in ("status", "history"):
        return None
    if verb == "auth" and second in ("can-i", "whoami"):
        return None
    if verb == "cluster-info" and second != "dump":
        return None
    if verb == "config":
        if second in ("current-context", "get-contexts", "get-clusters"):
            return None
        if second == "view" and not any(arg.split("=")[0] in ("--raw", "--flatten") for arg in args):
            return None
    return f"{base} {verb} {second}".rstrip() + " isn't a read-only command"


def output_format(args):
    value = None
    for index, arg in enumerate(args):
        if arg in ("-o", "--output") and index + 1 < len(args):
            value = args[index + 1]
        elif arg.startswith("--output="):
            value = arg[len("--output=") :]
        elif arg.startswith("-o") and not arg.startswith("--"):
            value = arg[2:].lstrip("=")
        elif arg.split("=")[0] in ("--template", "--go-template"):
            value = "template"
    return value


def check_kubectl_get(args, resources):
    raw = flag_value(args, "--raw")
    if raw is not None:
        if DYNAMIC in raw:
            return "the --raw path comes from a variable"
        if re.search(r"/secrets(/|\?|$)", unquote(raw)):
            return "it reads Secret values"
        return None
    if output_format(args) in SAFE_SECRET_OUTPUTS:
        return None
    if not resources:
        return "the guard can't tell what this reads"
    if DYNAMIC in resources[0]:
        return "the resource type comes from a variable, and the output format would show Secret values"
    for resource in resources:
        for part in resource.split(","):
            if part.split("/")[0].split(".")[0].lower() in ("secret", "secrets"):
                return "it reads Secret values"
    return None


def check_kubectl_exec(args):
    if "--" not in args:
        return "kubectl exec needs -- before the command, so the guard can read it"
    command = args[args.index("--") + 1 :]
    if not command:
        return "kubectl exec has no command"
    name = os.path.basename(command[0])
    if name in EXEC_LOOKUPS:
        return None
    if name == "cat" and command[1:] and all(path in EXEC_READABLE_FILES for path in command[1:]):
        return None
    return f"kubectl exec is limited to DNS lookups and resolver files, not {name}"


def check_table(base, args):
    words = positionals(args, VALUE_FLAGS)
    if not words:
        return None
    first = words[0]
    if base in ("gh", "glab") and first == "api":
        return check_forge_api(args)
    table = TOOL_TABLES[base]
    if first not in table:
        return f"{base} {first} isn't a read-only command"
    allowed = table[first]
    if allowed is None:
        return None
    second = words[1] if len(words) > 1 else ""
    if second in allowed:
        return None
    return f"{base} {first} {second}".rstrip() + " isn't a read-only command"


def check_forge_api(args):
    for index, arg in enumerate(args):
        name = arg.split("=", 1)[0]
        method = None
        if name in ("-X", "--method"):
            method = arg.split("=", 1)[1] if "=" in arg else (args[index + 1] if index + 1 < len(args) else "")
        elif arg.startswith("-X"):
            method = arg[2:]
        if method is not None and method.upper() != "GET":
            return "the API call isn't a GET"
        if name in ("-f", "-F", "--field", "--raw-field", "--input"):
            return "sending fields turns the API call into a POST"
    return None


def check_aws(args):
    words = positionals(args, VALUE_FLAGS)
    if len(words) < 2:
        return "the guard can't tell what this aws call does"
    service, operation = words[0], words[1]
    if "--with-decryption" in args or AWS_SECRET_OPERATION.search(operation):
        return "it reads a secret or a credential"
    if (service, operation) in AWS_OTHER_READS or AWS_READ_OPERATION.match(operation):
        return None
    return f"aws {service} {operation} isn't a read-only call"


def check_gcloud(args):
    words = [arg for arg in args if not arg.startswith("-")]
    for word in words:
        if GCLOUD_WRITE_VERB.match(word):
            return f"gcloud {word} isn't a read-only call"
    if any(GCLOUD_READ_VERB.match(word) for word in words):
        return None
    return "the guard can't find a read-only verb in this gcloud call"


def check_git(args):
    rest = list(args)
    while rest and rest[0].startswith("-"):
        flag = rest.pop(0)
        if flag == "-c" or flag.startswith("--exec-path") or flag.startswith("--config-env"):
            return "git -c can run commands"
        if flag in GIT_VALUE_FLAGS and rest:
            rest.pop(0)
    if not rest:
        return None
    subcommand, tail = rest[0], rest[1:]
    if subcommand not in GIT_READ:
        return f"git {subcommand} isn't a read-only command"
    if any(arg.split("=")[0] in ("--output", "--ext-diff", "--open-files-in-pager") or arg.startswith("-O") for arg in tail):
        return "it writes a file or runs an external program"
    if subcommand == "remote" and not (not tail or tail[0] in ("-v", "--verbose", "show", "get-url")):
        return "it changes a remote"
    if subcommand == "reflog" and tail and tail[0] in ("expire", "delete"):
        return "it changes the reflog"
    if subcommand == "config" and not any(
        arg in ("-l", "--list") or arg.startswith("--get") for arg in tail
    ):
        return "it changes git config"
    if subcommand == "branch" and not all(
        arg.startswith("-") and arg not in ("-d", "-D", "-m", "-M", "-c", "-C", "-f", "--delete", "--move", "--copy", "--force")
        or "--list" in tail or "--contains" in tail
        for arg in tail
    ):
        return "it changes a branch"
    if subcommand == "tag" and not any(
        arg in ("-l", "--list", "--contains", "--points-at") or arg.startswith("--list") for arg in tail
    ):
        return "it changes a tag"
    return None


def check_curl(args):
    forced_get = False
    has_body = False
    method = None
    urls = []
    index = 0
    while index < len(args):
        arg = args[index]
        index += 1
        if arg.startswith("--"):
            name, has_inline, inline = arg.partition("=")

            def value():
                nonlocal index
                if has_inline:
                    return inline
                index += 1
                return args[index - 1] if index <= len(args) else ""

            if name == "--get":
                forced_get = True
            elif name == "--request":
                method = value()
            elif name == "--url":
                urls.append(value())
            elif name in CURL_BODY_FLAGS:
                has_body = True
                value()
            elif name == "--output":
                if value() not in HARMLESS_REDIRECTS:
                    return "curl would write a file"
            elif name in CURL_WRITE_FLAGS:
                return f"curl {name} writes or sends a file"
            elif name in CURL_LONG_VALUE_FLAGS:
                value()
        elif arg.startswith("-") and len(arg) > 1:
            position = 1
            while position < len(arg):
                flag = arg[position]
                attached = arg[position + 1 :]
                position += 1
                if flag == "G":
                    forced_get = True
                elif flag in CURL_SHORT_WRITE_FLAGS:
                    return f"curl -{flag} writes or sends a file"
                elif flag in "Xdo" or flag in CURL_SHORT_VALUE_FLAGS:
                    if attached:
                        taken = attached
                    else:
                        taken = args[index] if index < len(args) else ""
                        index += 1
                    if flag == "X":
                        method = taken
                    elif flag == "d":
                        has_body = True
                    elif flag == "o" and taken not in HARMLESS_REDIRECTS:
                        return "curl would write a file"
                    break
        else:
            urls.append(arg)

    if method is not None and method.upper() not in ("GET", "HEAD", "POST"):
        return f"curl {method} isn't a read"
    sends = has_body and not forced_get or (method or "").upper() == "POST"
    if sends and not any(SEARCH_READ_PATH.search(url) for url in urls):
        return "curl would send data, and this isn't a search endpoint"
    return None


def check_reader(base, args):
    if base in PRINTS_ENVIRONMENT_WITHOUT_ARGS and not args:
        return f"{base} with no arguments prints the environment"
    if any(CREDENTIAL_PATH.search(arg) for arg in args):
        return "it reads a credential file"
    if base in PRINTS_ENVIRONMENT_WITHOUT_ARGS:
        for arg in args:
            match = ASSIGNMENT.match(arg)
            if match and (match.group(1) in DANGEROUS_ENV or match.group(1).startswith(("LD_", "DYLD_"))):
                return f"it sets {match.group(1)}"
    if base == "rg" and any(arg.split("=")[0] == "--pre" for arg in args):
        return "rg --pre runs a command"
    if base == "sed" and any(
        arg.startswith("-i") or arg.startswith("--in-place") or arg.startswith("-f") or SED_RUNS_OR_WRITES.search(arg)
        for arg in args
    ):
        return "sed would edit a file or run a command"
    if base == "sort" and any(arg.startswith("-o") or arg.startswith("--output") or arg.startswith("--compress-program") for arg in args):
        return "sort would write a file"
    if base == "find" and any(
        arg in ("-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls") for arg in args
    ):
        return "find would change something or run a command"
    if base == "awk" and any(
        "system(" in arg or "getline" in arg or "|" in arg and '"' in arg or "print >" in arg or "printf >" in arg
        for arg in args
    ):
        return "awk would run a command or write a file"
    return None


def check_path(path):
    if CREDENTIAL_PATH.search(str(path)):
        return "it reads a credential file"
    return None


def main():
    event = json.load(sys.stdin)
    agent = str(event.get("agent_type") or "").rsplit(":", 1)[-1]
    if agent not in INVESTIGATORS:
        return 0

    tool = event.get("tool_name")
    tool_input = event.get("tool_input") or {}
    if tool == "Bash":
        reason = check_command_line(tool_input["command"])
    elif tool == "Read":
        reason = check_path(tool_input.get("file_path", ""))
    elif tool == "Grep":
        reason = check_path(tool_input.get("path", ""))
    else:
        reason = None

    if reason:
        print(
            f"sre-agent read-only guard blocked this: {reason}. Record it under GAPS and move on. "
            "Don't look for another way to run it.",
            file=sys.stderr,
        )
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
