package main

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// The schemas in schema/ are artifacts rather than code: nothing embeds them
// and no binary reads them, which is the convention the sibling weaverbird
// repo uses for the same job. What keeps them honest is this file, which
// walks each schema and the struct it claims to describe together and fails
// when either grows a field the other does not have. A schema that has
// drifted is worse than no schema, because an editor keeps asserting it.
//
// Read from disk rather than embedded for the same reason: a test that has to
// go and find the published file cannot pass against a stale copy compiled in
// beside it.

// schemaDir is where the published schemas live, relative to this package.
const schemaDir = "../../schema/"

func read(t *testing.T, file string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return doc
}

func TestSchemasDescribeTheStructsTheyClaimTo(t *testing.T) {
	cases := []struct {
		file string
		typ  reflect.Type
	}{
		{"project-config.schema.json", reflect.TypeOf(projectconfig.Config{})},
		{"config.schema.json", reflect.TypeOf(config.Config{})},
		{"extractor.schema.json", reflect.TypeOf(usage.Extractor{})},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			compare(t, "", read(t, schemaDir+c.file), c.typ)
		})
	}
}

// compare checks one object node against one struct type, then descends.
func compare(t *testing.T, path string, node map[string]any, typ reflect.Type) {
	t.Helper()
	props, _ := node["properties"].(map[string]any)
	if props == nil {
		t.Errorf("%s: schema node has no properties", where(path))
		return
	}

	fields := jsonFields(typ)
	delete(fields, "$schema")
	// "$schema" is a pointer for editors rather than a value bloodhound
	// reads, so a schema may carry it whether or not the struct does.
	inSchema := map[string]bool{}
	for k := range props {
		if k != "$schema" {
			inSchema[k] = true
		}
	}

	for name := range fields {
		if !inSchema[name] {
			t.Errorf("%s: struct has %q, schema does not", where(path), path+name)
		}
	}
	for name := range inSchema {
		if _, ok := fields[name]; !ok {
			t.Errorf("%s: schema has %q, struct does not", where(path), path+name)
		}
	}

	// additionalProperties must stay closed, or an unknown key validates
	// clean and the schema stops catching the mistake it exists for.
	if allow, ok := node["additionalProperties"]; !ok || allow != false {
		t.Errorf("%s: additionalProperties is not false", where(path))
	}

	for name, ft := range fields {
		child, _ := props[name].(map[string]any)
		if child == nil {
			continue
		}
		switch ft.Kind() {
		case reflect.Struct:
			compare(t, path+name+".", child, ft)
		case reflect.Slice:
			if ft.Elem().Kind() != reflect.Struct {
				continue
			}
			items, _ := child["items"].(map[string]any)
			if items == nil {
				t.Errorf("%s: %q is a list of objects with no items schema", where(path), path+name)
				continue
			}
			compare(t, path+name+"[].", items, ft.Elem())
		}
	}
}

func where(path string) string {
	if path == "" {
		return "root"
	}
	return strings.TrimSuffix(path, ".")
}

// jsonFields is the struct's own account of its wire shape. Unexported fields
// and those tagged "-" are not part of it.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

// TestEveryEnumIsSpelledOut catches the other half of drift: a schema whose
// keys are right and whose allowed values have fallen behind the constants.
func TestEveryEnumIsSpelledOut(t *testing.T) {
	cases := []struct {
		file string
		path []string
		want []string
	}{
		{"project-config.schema.json", []string{"wakeup", "mode"},
			[]string{projectconfig.WakeupOff, projectconfig.WakeupNudge, projectconfig.WakeupResume}},
		{"config.schema.json", []string{"self_heal_mode"},
			[]string{config.SelfHealModeInteractive, config.SelfHealModeHeadless}},
		{"config.schema.json", []string{"trail_mode"},
			[]string{config.TrailModeInteractive, config.TrailModeHeadless}},
		{"config.schema.json", []string{"plan_tier"},
			[]string{config.PlanUnknown, config.PlanPro, config.PlanMax5, config.PlanMax20}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.path, "."), func(t *testing.T) {
			node := read(t, schemaDir+c.file)
			for _, step := range c.path {
				props, _ := node["properties"].(map[string]any)
				next, _ := props[step].(map[string]any)
				if next == nil {
					t.Fatalf("no schema node at %q", step)
				}
				node = next
			}
			raw, _ := node["enum"].([]any)
			got := make([]string, 0, len(raw))
			for _, v := range raw {
				s, _ := v.(string)
				got = append(got, s)
			}
			sort.Strings(got)
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("enum = %v, want %v", got, want)
			}
		})
	}
}

// TestEverySchemaKnowsWhereItIsPublished pins $id to the place a checked-in
// "$schema" pointer resolves to. A schema that has moved and not said so is
// one every editor silently stops fetching.
func TestEverySchemaKnowsWhereItIsPublished(t *testing.T) {
	const base = "https://raw.githubusercontent.com/PeterSR/claude-code-bloodhound/main/schema/"
	for _, file := range []string{
		"project-config.schema.json",
		"config.schema.json",
		"extractor.schema.json",
	} {
		t.Run(file, func(t *testing.T) {
			doc := read(t, schemaDir+file)
			want := base + file
			if got, _ := doc["$id"].(string); got != want {
				t.Errorf("$id = %q, want %q", got, want)
			}
		})
	}
}
