package discover

import (
	"fmt"
	"os"
	"strings"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

type Environment struct {
	Tools             map[string]bool `json:"tools"`
	Context           string          `json:"context,omitempty"`
	ClientVersion     string          `json:"client_version,omitempty"`
	ServerVersion     string          `json:"server_version,omitempty"`
	Namespaces        []string        `json:"namespaces,omitempty"`
	ProviderIDs       []string        `json:"provider_ids,omitempty"`
	EKS               *bool           `json:"eks,omitempty"`
	GKE               *bool           `json:"gke,omitempty"`
	Flux              *bool           `json:"flux,omitempty"`
	ArgoCD            *bool           `json:"argocd,omitempty"`
	Crossplane        *bool           `json:"crossplane,omitempty"`
	AzureSubscription string          `json:"azure_subscription,omitempty"`
}

type Observability struct {
	Context    string     `json:"context,omitempty"`
	Endpoints  []Endpoint `json:"endpoints,omitempty"`
	Ingresses  []Ingress  `json:"ingresses,omitempty"`
	Istio      *bool      `json:"istio,omitempty"`
	Linkerd    *bool      `json:"linkerd,omitempty"`
	K6CLI      bool       `json:"k6_cli"`
	K6Version  string     `json:"k6_version,omitempty"`
	K6Operator *bool      `json:"k6_operator,omitempty"`
}

type Endpoint struct {
	Component    string   `json:"component"`
	Namespace    string   `json:"namespace"`
	Service      string   `json:"service"`
	Ports        []int    `json:"ports"`
	PortForward  []string `json:"port_forward"`
	ReadinessURL string   `json:"readiness_url"`
}

type Ingress struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Hosts     []string `json:"hosts"`
}

type AWS struct {
	Account string `json:"account,omitempty"`
	ARN     string `json:"arn,omitempty"`
	Profile string `json:"profile,omitempty"`
	Region  string `json:"region,omitempty"`
}

type GCloud struct {
	Account  string    `json:"account,omitempty"`
	Project  string    `json:"project,omitempty"`
	Region   string    `json:"region,omitempty"`
	Zone     string    `json:"zone,omitempty"`
	Projects []Project `json:"projects,omitempty"`
}

type Project struct {
	ID    string `json:"projectId"`
	Name  string `json:"name"`
	State string `json:"lifecycleState"`
}

type GitHub struct {
	NameWithOwner string `json:"nameWithOwner,omitempty"`
	URL           string `json:"url,omitempty"`
}

type resource struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ProviderID string `json:"providerID"`
		Ports      []struct {
			Port int `json:"port"`
		} `json:"ports"`
		Rules []struct {
			Host string `json:"host"`
		} `json:"rules"`
	} `json:"spec"`
}
type resourceList struct {
	Items []resource `json:"items"`
}

func (p *probe) list(context string, args ...string) ([]resource, bool) {
	var list resourceList
	if !p.kube(&list, context, append([]string{"get"}, append(args, "-o", "json")...)...) {
		return nil, false
	}
	if list.Items == nil {
		p.fail(Error, "kubectl list: missing items array")
		return nil, false
	}
	return list.Items, true
}
func has(items []resource, name string) bool {
	for _, item := range items {
		if item.Metadata.Name == name {
			return true
		}
	}
	return false
}
func boolean(v bool) *bool { return &v }
func (p *probe) safe(s string) string {
	if err := p.err(); err != nil {
		p.check("external scalar", executil.Result{Err: err})
		return ""
	}
	value, err := safety.NewRedactor().RedactContext(p.ctx, s)
	if err == nil {
		err = p.err()
	}
	if err != nil {
		p.check("external scalar", executil.Result{Err: err})
		return ""
	}
	return value
}

func (p *probe) environment() {
	e := &Environment{Tools: map[string]bool{}}
	p.result.Findings.Env = e
	for _, name := range []string{"kubectl", "helm", "kustomize", "flux", "argocd", "git", "gh", "glab", "aws", "gcloud", "az", "terraform", "pulumi", "crossplane", "jq", "curl"} {
		e.Tools[name] = p.have(name)
	}
	ctx := p.cluster()
	if ctx != "" {
		e.Context = p.safe(ctx)
		var version struct {
			ClientVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"clientVersion"`
			ServerVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"serverVersion"`
		}
		if p.kube(&version, ctx, "version", "-o", "json") {
			e.ClientVersion = version.ClientVersion.GitVersion
			e.ServerVersion = version.ServerVersion.GitVersion
			if e.ClientVersion == "" || e.ServerVersion == "" {
				p.fail(Error, "kubectl version: missing version fields")
			}
		}
		if namespaces, ok := p.list(ctx, "namespaces"); ok {
			for _, ns := range namespaces {
				e.Namespaces = append(e.Namespaces, ns.Metadata.Name)
			}
		}
		nodes, nodesOK := p.list(ctx, "nodes")
		aws, gce, gkeLabel := false, false, false
		if nodesOK {
			seen := map[string]bool{}
			for _, node := range nodes {
				if p.err() != nil {
					return
				}
				id := node.Spec.ProviderID
				if id != "" && !seen[id] {
					if len(e.ProviderIDs) < 20 {
						e.ProviderIDs = append(e.ProviderIDs, id)
					}
					seen[id] = true
				}
				aws = aws || strings.HasPrefix(id, "aws://")
				gce = gce || strings.HasPrefix(id, "gce://")
				_, label := node.Metadata.Labels["cloud.google.com/gke-nodepool"]
				gkeLabel = gkeLabel || label
			}
			if len(seen) > len(e.ProviderIDs) {
				p.result.Gaps = append(p.result.Gaps, fmt.Sprintf("provider ID sample: %d additional IDs omitted; all nodes inspected", len(seen)-len(e.ProviderIDs)))
			}
			e.GKE = boolean(gce && gkeLabel)
		}
		ds, dsOK := p.list(ctx, "daemonsets", "-n", "kube-system")
		if nodesOK && dsOK {
			e.EKS = boolean(aws && has(ds, "aws-node"))
		}
		if crds, ok := p.list(ctx, "crds"); ok {
			e.Flux = boolean(has(crds, "kustomizations.kustomize.toolkit.fluxcd.io"))
			e.ArgoCD = boolean(has(crds, "applications.argoproj.io"))
			e.Crossplane = boolean(has(crds, "providers.pkg.crossplane.io"))
		}
	}
	if !p.opts.SkipCluster && p.have("az") {
		var account struct {
			Name string `json:"name"`
		}
		if p.read("Azure account", &account, "az", "account", "show", "-o", "json") {
			e.AzureSubscription = account.Name
			if account.Name == "" {
				p.fail(Error, "Azure account: missing subscription name")
			}
		}
	}
}

var components = []struct {
	name string
	port int
	path string
}{
	{"prometheus", 9090, "/-/ready"}, {"alertmanager", 9093, "/-/ready"}, {"grafana", 3000, "/api/health"},
	{"loki", 3100, "/ready"}, {"mimir", 9009, "/ready"}, {"tempo", 3200, "/ready"}, {"jaeger", 16686, "/"},
	{"elasticsearch", 9200, "/_cluster/health"}, {"opensearch", 9200, "/_cluster/health"},
}

func (p *probe) observability() {
	o := &Observability{K6CLI: p.have("k6")}
	p.result.Findings.Obs = o
	ctx := p.cluster()
	if ctx == "" {
		return
	}
	o.Context = p.safe(ctx)
	if services, ok := p.list(ctx, "services", "--all-namespaces"); ok {
		for _, svc := range services {
			if p.err() != nil {
				return
			}
			name := strings.ToLower(svc.Metadata.Name)
			for _, c := range components {
				matches := strings.Contains(name, c.name)
				if c.name == "jaeger" {
					start := strings.Index(name, "jaeger")
					matches = name == "jaeger" || (start >= 0 && strings.Contains(name[start+len("jaeger"):], "query"))
				}
				if !matches {
					continue
				}
				ports := []int{}
				for _, port := range svc.Spec.Ports {
					if port.Port > 0 {
						ports = append(ports, port.Port)
					}
				}
				if len(ports) == 0 {
					p.fail(Error, "observability service has no exposed port")
					continue
				}
				o.Endpoints = append(o.Endpoints, Endpoint{Component: c.name, Namespace: svc.Metadata.Namespace, Service: svc.Metadata.Name, Ports: ports,
					PortForward:  []string{"kubectl", "--context", o.Context, "port-forward", "-n", svc.Metadata.Namespace, "svc/" + svc.Metadata.Name, fmt.Sprintf("%d:%d", c.port, ports[0])},
					ReadinessURL: fmt.Sprintf("http://localhost:%d%s", c.port, c.path)})
			}
		}
	}
	namespaces, nsOK := p.list(ctx, "namespaces")
	crds, crdOK := p.list(ctx, "crds")
	if has(namespaces, "istio-system") || has(crds, "virtualservices.networking.istio.io") {
		o.Istio = boolean(true)
	} else if nsOK && crdOK {
		o.Istio = boolean(false)
	}
	if has(namespaces, "linkerd") || has(crds, "serviceprofiles.linkerd.io") {
		o.Linkerd = boolean(true)
	} else if nsOK && crdOK {
		o.Linkerd = boolean(false)
	}
	if crdOK {
		o.K6Operator = boolean(has(crds, "testruns.k6.io"))
	}
	if o.K6CLI {
		r := p.run("k6", "version")
		if p.check("k6 version", r) {
			first, _, _ := strings.Cut(strings.TrimSpace(r.Stdout), "\n")
			o.K6Version = p.safe(first)
		}
	}
	if ingresses, ok := p.list(ctx, "ingresses", "--all-namespaces"); ok {
		for _, ing := range ingresses {
			if p.err() != nil {
				return
			}
			hosts := []string{}
			search := ing.Metadata.Name + " " + ing.Metadata.Namespace
			for _, rule := range ing.Spec.Rules {
				if rule.Host != "" {
					hosts = append(hosts, rule.Host)
					search += " " + rule.Host
				}
			}
			search = strings.ToLower(search)
			matches := strings.Contains(search, "kibana") || strings.Contains(search, "metrics")
			for _, c := range components {
				matches = matches || strings.Contains(search, c.name)
			}
			if matches {
				o.Ingresses = append(o.Ingresses, Ingress{Namespace: ing.Metadata.Namespace, Name: ing.Metadata.Name, Hosts: hosts})
			}
		}
	}
}

func (p *probe) aws() {
	a := &AWS{}
	p.result.Findings.AWS = a
	if !p.require("aws") || !p.online() {
		return
	}
	a.Profile = os.Getenv("AWS_PROFILE")
	if a.Profile == "" {
		a.Profile = os.Getenv("AWS_DEFAULT_PROFILE")
	}
	if a.Profile == "" {
		a.Profile = "default"
	}
	a.Profile = p.safe(a.Profile)
	var identity struct {
		Account string `json:"Account"`
		ARN     string `json:"Arn"`
	}
	if p.read("AWS identity", &identity, "aws", "sts", "get-caller-identity", "--output", "json", "--no-cli-pager", "--no-cli-auto-prompt") {
		a.Account = identity.Account
		a.ARN = identity.ARN
		if a.Account == "" || a.ARN == "" {
			p.fail(Error, "AWS identity: missing account or ARN")
		}
	}
	a.Region = os.Getenv("AWS_REGION")
	if a.Region == "" {
		a.Region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if a.Region == "" {
		r := p.run("aws", "configure", "get", "region")
		if r.Err != nil && r.ExitCode != nil && *r.ExitCode == 1 && r.Stdout == "" && r.Stderr == "" {
			p.fail(Missing, "AWS region is not configured")
		} else if p.check("AWS region", r) {
			a.Region = strings.TrimSpace(r.Stdout)
		}
	}
	a.Region = p.safe(a.Region)
	if a.Region == "" && p.result.Status == OK {
		p.fail(Missing, "AWS region is not configured")
	}
}
func (p *probe) gcloud() {
	g := &GCloud{}
	p.result.Findings.GCloud = g
	if !p.require("gcloud") || !p.online() {
		return
	}
	var accounts []struct {
		Account string `json:"account"`
		Status  string `json:"status"`
	}
	if p.read("GCP active account", &accounts, "gcloud", "auth", "list", "--filter=status:ACTIVE", "--format=json", "--quiet") {
		for _, a := range accounts {
			if a.Status == "ACTIVE" {
				g.Account = a.Account
				break
			}
		}
		if g.Account == "" {
			p.fail(Missing, "GCP active account unavailable")
		}
	}
	var config struct {
		Core struct {
			Project string `json:"project"`
		} `json:"core"`
		Compute struct {
			Region string `json:"region"`
			Zone   string `json:"zone"`
		} `json:"compute"`
	}
	if p.read("GCP config", &config, "gcloud", "config", "list", "--format=json", "--quiet") {
		g.Project = config.Core.Project
		g.Region = config.Compute.Region
		g.Zone = config.Compute.Zone
		if g.Project == "" {
			p.fail(Missing, "GCP project is not configured")
		}
	}
	var projects []Project
	if p.read("GCP accessible projects", &projects, "gcloud", "projects", "list", "--limit=20", "--format=json", "--quiet") {
		g.Projects = projects
		if len(g.Projects) > 20 {
			g.Projects = g.Projects[:20]
			p.result.Gaps = append(p.result.Gaps, "accessible projects limited to 20")
		}
	}
}
func (p *probe) github() {
	g := &GitHub{}
	p.result.Findings.GitHub = g
	if !p.require("gh") || !p.online() {
		return
	}
	var repository GitHub
	if !p.read("GitHub repository", &repository, "gh", "repo", "view", "--json", "nameWithOwner,url") {
		return
	}
	if repository.NameWithOwner == "" || repository.URL == "" {
		p.fail(Error, "GitHub repository: missing name or URL")
		return
	}
	*g = repository
}
