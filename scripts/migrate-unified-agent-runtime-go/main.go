package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func main() {
	input := flag.String("input", "", "old-binary bundle export")
	output := flag.String("output", "", "new unified bundle")
	routeMapPath := flag.String("route-map", "", "old-to-new route ID map")
	flag.Parse()
	if *input == "" || *output == "" || *routeMapPath == "" {
		fatal(errors.New("--input, --output, and --route-map are required"))
	}
	if *input == *output || *output == *routeMapPath || *input == *routeMapPath {
		fatal(errors.New("input, output, and route-map paths must be distinct"))
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		fatal(err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		fatal(fmt.Errorf("decode input: %w", err))
	}
	routeMap, err := migrate(root)
	if err != nil {
		fatal(err)
	}
	out, err := yaml.Marshal(root)
	if err != nil {
		fatal(err)
	}
	mapDoc := map[string]any{"apiVersion": "gateway.agw/migration-v1", "kind": "UnifiedAgentRuntimeRouteMap", "routes": routeMap}
	mapBytes, err := yaml.Marshal(mapDoc)
	if err != nil {
		fatal(err)
	}
	if err := writePair(*output, out, *routeMapPath, mapBytes); err != nil {
		fatal(err)
	}
}

func migrate(root map[string]any) (map[string]string, error) {
	agents := objectList(root["agents"])
	services := objectList(root["acpServices"])
	acpRoutes := objectList(root["acpRoutes"])
	builtinRoutes := objectList(root["builtinRoutes"])
	serviceByID := indexByID(services)
	agentByID := indexByID(agents)
	if dup := duplicateIDs(services); len(dup) > 0 {
		return nil, fmt.Errorf("duplicate ACP service IDs: %s", strings.Join(dup, ", "))
	}
	if dup := duplicateIDs(agents); len(dup) > 0 {
		return nil, fmt.Errorf("duplicate Agent IDs: %s", strings.Join(dup, ", "))
	}
	bindings := map[string][]map[string]any{}
	for _, a := range agents {
		runtime := object(a["runtime"])
		if stringValue(runtime["type"]) != "acp" {
			delete(a, "owns_service")
			continue
		}
		acp := object(runtime["acp"])
		sid := stringValue(acp["service_id"])
		if sid == "" {
			return nil, fmt.Errorf("agent %q has ACP runtime without service_id", stringValue(a["id"]))
		}
		bindings[sid] = append(bindings[sid], a)
	}
	var bindingErrors []string
	for id := range serviceByID {
		owners := bindings[id]
		if len(owners) != 1 {
			bindingErrors = append(bindingErrors, fmt.Sprintf("service %q owners=%v", id, agentIDs(owners)))
		}
	}
	for sid, owners := range bindings {
		if _, ok := serviceByID[sid]; !ok || len(owners) != 1 {
			bindingErrors = append(bindingErrors, fmt.Sprintf("service %q owners=%v", sid, agentIDs(owners)))
		}
	}
	if len(bindingErrors) > 0 {
		sort.Strings(bindingErrors)
		return nil, fmt.Errorf("legacy_agent_runtime_config: cannot migrate orphan, missing, or multiply-bound ACP services: %s. Inspect with old-agwctl gateway acp-service get <service-id> and old-agwctl gateway acp-route list; then either delete its routes/service after export, or bind it to exactly one Agent in an exported bundle and run old-agwctl gateway validate/apply before re-exporting", strings.Join(bindingErrors, "; "))
	}
	for sid, owners := range bindings {
		a, svc := owners[0], serviceByID[sid]
		runtime := object(a["runtime"])
		inline := map[string]any{}
		for _, key := range []string{"agent_type", "cwd", "allowed_roots", "default_model", "env", "config_overrides", "idle_ttl", "max_instances", "permission_mode", "codex"} {
			if v, ok := svc[key]; ok {
				inline[key] = v
			}
		}
		runtime["acp"] = inline
		a["runtime"] = runtime
		if boolValue(a["disabled"]) || boolValue(svc["disabled"]) {
			a["disabled"] = true
		}
		delete(a, "owns_service")
		if routes := object(a["routes"]); routes != nil {
			delete(routes, "acp_route_ids")
			delete(routes, "builtin_route_ids")
			a["routes"] = routes
		}
	}
	ownerByService := map[string]string{}
	for sid, owners := range bindings {
		ownerByService[sid] = stringValue(owners[0]["id"])
	}
	var unified []any
	routeMap := map[string]string{}
	ids := map[string]string{}
	for _, family := range []string{"llmRoutes", "mcpRoutes"} {
		for _, route := range objectList(root[family]) {
			id := stringValue(route["id"])
			if id == "" {
				continue
			}
			if prior, exists := ids[id]; exists {
				return nil, fmt.Errorf("route ID collision %q from %s and %s", id, prior, family)
			}
			ids[id] = family
		}
	}
	convert := func(route map[string]any, targetID, oldPrefix, oldTarget string) error {
		oldID := stringValue(route["id"])
		path := stringValue(object(route["match_policy"])["path_prefix"])
		if oldID == "" {
			oldID = routeID(oldPrefix, oldTarget, path)
		}
		newID := oldID
		if oldID == routeID(oldPrefix, oldTarget, path) {
			newID = routeID("agent", targetID, path)
		}
		if prior, exists := ids[newID]; exists {
			return fmt.Errorf("route ID collision %q from %q and %q", newID, prior, oldID)
		}
		ids[newID], routeMap[oldID] = oldID, newID
		route["id"], route["kind"], route["protocol"], route["agent_id"] = newID, "agent", "agent", targetID
		delete(route, "service_id")
		delete(route, "created_at")
		delete(route, "updated_at")
		unified = append(unified, route)
		return nil
	}
	for _, route := range acpRoutes {
		sid := stringValue(route["service_id"])
		target := ownerByService[sid]
		if target == "" {
			return nil, fmt.Errorf("ACP route %q references unbound service %q", stringValue(route["id"]), sid)
		}
		if err := convert(route, target, "acp", sid); err != nil {
			return nil, err
		}
	}
	for _, route := range builtinRoutes {
		target := stringValue(route["agent_id"])
		if target == "" {
			return nil, fmt.Errorf("builtin route %q has no agent_id", stringValue(route["id"]))
		}
		if _, ok := agentByID[target]; !ok {
			return nil, fmt.Errorf("builtin route %q references missing agent %q", stringValue(route["id"]), target)
		}
		if err := convert(route, target, "builtin", target); err != nil {
			return nil, err
		}
	}
	root["agentRoutes"] = unified
	delete(root, "acpServices")
	delete(root, "acpRoutes")
	delete(root, "builtinRoutes")
	known := map[string]struct{}{}
	for _, family := range []string{"llmRoutes", "mcpRoutes"} {
		for _, route := range objectList(root[family]) {
			known[stringValue(route["id"])] = struct{}{}
		}
	}
	for id := range ids {
		known[id] = struct{}{}
	}
	for _, vk := range objectList(root["virtualKeys"]) {
		refs := stringList(vk["allowed_route_ids"])
		rewritten := make([]any, 0, len(refs))
		for _, id := range refs {
			if mapped, ok := routeMap[id]; ok {
				id = mapped
			}
			if _, ok := known[id]; !ok {
				return nil, fmt.Errorf("VirtualKey %q references unresolved route %q", stringValue(vk["id"]), id)
			}
			rewritten = append(rewritten, id)
		}
		if len(rewritten) > 0 {
			vk["allowed_route_ids"] = rewritten
		}
	}
	return routeMap, nil
}

func routeID(prefix, target, path string) string {
	slug := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(strings.TrimSpace(path)), "-"), "-")
	if slug == "" {
		slug = "root"
	}
	return prefix + ":" + strings.TrimSpace(target) + ":" + slug
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func objectList(v any) []map[string]any {
	xs, _ := v.([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if m := object(x); m != nil {
			out = append(out, m)
		}
	}
	return out
}
func stringValue(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }
func boolValue(v any) bool     { b, _ := v.(bool); return b }
func stringList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s := stringValue(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}
func indexByID(xs []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, x := range xs {
		id := stringValue(x["id"])
		if id != "" {
			out[id] = x
		}
	}
	return out
}
func duplicateIDs(xs []map[string]any) []string {
	seen := map[string]bool{}
	dup := map[string]bool{}
	for _, x := range xs {
		id := stringValue(x["id"])
		if id != "" {
			if seen[id] {
				dup[id] = true
			}
			seen[id] = true
		}
	}
	out := make([]string, 0, len(dup))
	for id := range dup {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func agentIDs(xs []map[string]any) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, stringValue(x["id"]))
	}
	sort.Strings(out)
	return out
}
func writePair(a string, adata []byte, b string, bdata []byte) error {
	if err := os.MkdirAll(filepath.Dir(a), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b), 0755); err != nil {
		return err
	}
	af, err := os.CreateTemp(filepath.Dir(a), ".unified-bundle-*")
	if err != nil {
		return err
	}
	an := af.Name()
	defer os.Remove(an)
	bf, err := os.CreateTemp(filepath.Dir(b), ".unified-route-map-*")
	if err != nil {
		af.Close()
		return err
	}
	bn := bf.Name()
	defer os.Remove(bn)
	if _, err = af.Write(adata); err == nil {
		err = af.Chmod(0644)
	}
	if closeErr := af.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		bf.Close()
		return err
	}
	if _, err = bf.Write(bdata); err == nil {
		err = bf.Chmod(0644)
	}
	if closeErr := bf.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(an, a); err != nil {
		return err
	}
	return os.Rename(bn, b)
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
