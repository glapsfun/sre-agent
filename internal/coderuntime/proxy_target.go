package coderuntime

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var errTarget = errors.New("invalid proxy target")

func normalizeAuthority(authority, defaultPort string) (string, error) {
	if authority == "" || len(authority) > 260 {
		return "", errTarget
	}
	for _, c := range authority {
		if c <= 32 || c >= 127 || strings.ContainsRune("/@?#%\\", c) {
			return "", errTarget
		}
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		port = defaultPort
		if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
			host = authority[1 : len(authority)-1]
		} else if !strings.Contains(authority, ":") {
			host = authority
		} else {
			return "", errTarget
		}
	}
	if host == "" || port == "" {
		return "", errTarget
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", errTarget
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", errTarget
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || strings.HasPrefix(authority, "[") && !ip.Is6() {
			return "", errTarget
		}
		host = ip.String()
	} else {
		if len(host) > 253 || strings.HasPrefix(authority, "[") {
			return "", errTarget
		}
		addressLike := true
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", errTarget
			}
			numeric := true
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return "", errTarget
				}
				if c < '0' || c > '9' {
					numeric = false
				}
			}
			// Reject legacy numeric addresses as the approval normalizer does.
			hexadecimal := len(label) > 2 && strings.EqualFold(label[:2], "0x")
			if hexadecimal {
				for _, c := range label[2:] {
					if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
						hexadecimal = false
						break
					}
				}
			}
			addressLike = addressLike && (numeric || hexadecimal)
		}
		if addressLike {
			return "", errTarget
		}
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}

func requestTarget(method, rawTarget, rawHost string) (string, error) {
	port := "80"
	authority := rawTarget
	if method == "CONNECT" {
		port = "443"
	} else {
		u, err := url.Parse(rawTarget)
		if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(rawTarget, "#") || !strings.HasPrefix(rawTarget, "http://") {
			return "", errTarget
		}
		authority = u.Host
	}
	target, err := normalizeAuthority(authority, port)
	if err != nil {
		return "", err
	}
	if rawHost != "" {
		host, err := normalizeAuthority(rawHost, port)
		if err != nil || host != target {
			return "", errTarget
		}
	}
	return target, nil
}
