package tools

import (
	"encoding/json"
	"testing"
)

func TestFilterSchemasNilIsFullSet(t *testing.T) {
	got, err := FilterSchemas(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(BuiltinSchemas()) {
		t.Fatalf("nil filter: got %d schemas, want %d", len(got), len(BuiltinSchemas()))
	}
}

func TestFilterSchemasKeepsOnlyListed(t *testing.T) {
	got, err := FilterSchemas([]string{"write_file", "read_file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "read_file" || got[1].Name != "write_file" {
		t.Fatalf("got %+v", got)
	}
}

func TestFilterSchemasUnknownNameIsError(t *testing.T) {
	if _, err := FilterSchemas([]string{"read_file", "shell"}); err == nil {
		t.Fatal("unknown tool name should be an error")
	}
}

func TestSchemasJSONRoundTrip(t *testing.T) {
	if SchemasJSON(BuiltinSchemas()) != CanonicalToolSchemasJSON() {
		t.Fatal("SchemasJSON(BuiltinSchemas()) must equal CanonicalToolSchemasJSON()")
	}
	got := ParseSchemasJSON(SchemasJSON([]ToolSchema{BuiltinSchemas()[0]}))
	if len(got) != 1 || got[0].Name != BuiltinSchemas()[0].Name {
		t.Fatalf("round trip: %+v", got)
	}
	if n := len(ParseSchemasJSON("")); n != len(BuiltinSchemas()) {
		t.Fatalf("empty JSON should mean the full built-in set, got %d", n)
	}
	if n := len(ParseSchemasJSON("[]")); n != 0 {
		t.Fatalf("[] should mean no tools, got %d", n)
	}
	var probe []map[string]any
	if err := json.Unmarshal([]byte(SchemasJSON(nil)), &probe); err != nil {
		t.Fatal(err)
	}
}
