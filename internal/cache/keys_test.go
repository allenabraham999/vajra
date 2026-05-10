package cache

import "testing"

// TestKeyShapes locks down the cache key namespace so a careless rename
// can't silently invalidate every running master at once.
func TestKeyShapes(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{SandboxStateKey("sb1"), "sandbox:sb1:state"},
		{NodeResourcesKey("n1"), "node:n1:resources"},
		{AccountSandboxCountKey("acc1"), "account:acc1:sandbox_count"},
		{TemplateKey("tmpl1"), "template:tmpl1"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("key %q != %q", c.got, c.want)
		}
	}
}
