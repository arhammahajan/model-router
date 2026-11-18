package router

import "testing"

func TestLlamaSuppressedRowsCannotPublishAliases(t *testing.T) {
	for _, collision := range []string{"canonical", "alias", "same-server"} {
		t.Run(collision, func(t *testing.T) {
			reg := NewRegistry(llamaCfg())
			first := []LlamaEntry{{Name: "winner", Upstream: "first", Aliases: []string{"claimed"}}}
			loser := LlamaEntry{Name: "winner", Upstream: "second", Aliases: []string{"leaked"}}
			if collision == "alias" {
				loser.Name = "claimed"
			}
			second := []LlamaEntry{loser}
			if collision == "same-server" {
				first = append(first, loser)
				second = nil
			}
			publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
				"llama-a": llamaLayer(LlamaReachable, first...),
				"llama-b": llamaLayer(LlamaReachable, second...),
			}}, 1000)
			if _, _, fault := reg.Current().Resolve("leaked"); fault != NotFound {
				t.Fatalf("suppressed row published an alias: %v", fault)
			}
			for _, name := range []string{"winner", "claimed"} {
				m, p, fault := reg.Current().Resolve(name)
				if fault != Found || m.Upstream != "first" || p.Name != "llama-a" {
					t.Fatalf("winner changed: model=%+v provider=%+v fault=%v", m, p, fault)
				}
			}
		})
	}
}
