package signature

import "testing"

func TestMaskVariables(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"numbers", "ERROR exit 137 latency=-12.5 ratio=+0.25 count=6e10", "ERROR exit <*> latency=<*> ratio=<*> count=<*>"},
		{"hex", "ERROR address 0xdeadBEEF digest deadbeef1234", "ERROR address <*> digest <*>"},
		{"uuid", "request 550e8400-e29b-41d4-a716-446655440000 failed", "request <*> failed"},
		{"ipv4", "WARN peer 192.168.10.2:8080 refused", "WARN peer <*>:<*> refused"},
		{"ipv6", "ERROR peer [2001:db8::1]:443 via ::ffff:192.0.2.1", "ERROR peer [<*>]:<*> via <*>"},
		{"timestamp", "2026-11-11T12:34:56.123456789+02:00 ERROR timeout", "<*> ERROR timeout"},
		{"space timestamp", "2026-11-11 12:34:56,789 WARN retry", "<*> WARN retry"},
		{"quotes", `ERROR user "alice\"smith" value 'it\'s secret'`, "ERROR user <*> value <*>"},
		{"pod", "ERROR checkout-6b7d8c9f6b-x2abc OOMKilled exit 137", "ERROR checkout-<*> OOMKilled exit <*>"},
		{"static", "WARN connection refused by backend", "WARN connection refused by backend"},
		{"json", `{"level":"error","message":"connection refused to 10.0.0.1","id":"abc"}`, `{"id":"<*>","level":"error","message":"connection refused to <*>"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Mask(tc.input); got != tc.want {
				t.Fatalf("Mask(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMaskRetainsErrorIdentity(t *testing.T) {
	a := Mask("checkout-6b7d8c9f6b-x2abc OOMKilled exit 137")
	b := Mask("checkout-789df567bb-abcde OOMKilled exit 143")
	if a != b {
		t.Fatalf("pod suffixes differ: %q vs %q", a, b)
	}
	if a == Mask("checkout-789df567bb-abcde connection refused exit 143") {
		t.Fatal("static error identities merged")
	}
}
