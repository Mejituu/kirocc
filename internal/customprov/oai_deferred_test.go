package customprov

import (
	"reflect"
	"strings"
	"testing"

	json "encoding/json/v2"
)

// assertToolsJSON compares the converted tools against exact JSON, so both
// order and field values are asserted.
func assertToolsJSON(t *testing.T, got []oaiTool, want string) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if string(b) != want {
		t.Errorf("tools mismatch:\n got: %s\nwant: %s", b, want)
	}
}

// oaiShapes renders each message as role plus its identifying detail, so
// sequence assertions stay readable.
func oaiShapes(msgs []oaiMessage) []string {
	shapes := make([]string, len(msgs))
	for i, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			ids := make([]string, len(m.ToolCalls))
			for j, call := range m.ToolCalls {
				ids[j] = call.ID
			}
			shapes[i] = "assistant:" + strings.Join(ids, ",")
		case m.Role == "tool":
			shapes[i] = "tool:" + m.ToolCallID
		case m.Role == "system" || m.Role == "user":
			if s, ok := m.Content.(string); ok {
				shapes[i] = m.Role + ":" + s
			} else {
				shapes[i] = m.Role + ":parts"
			}
		default:
			shapes[i] = m.Role
		}
	}
	return shapes
}

func assertShapes(t *testing.T, got []oaiMessage, want ...string) {
	t.Helper()
	if shapes := oaiShapes(got); !reflect.DeepEqual(shapes, want) {
		t.Errorf("message shapes = %v, want %v", shapes, want)
	}
}

// TestBuildOpenAIRequest_DeferredToolAddition covers rule 1: definitions from
// tool_addition blocks join the converted tools, original tools come first
// and win on a name collision, and the system text stays.
func TestBuildOpenAIRequest_DeferredToolAddition(t *testing.T) {
	raw := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":[
			{"type":"text","text":"MCP tools loaded."},
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"mcp__x__build","description":"Build things","input_schema":{"type":"object","properties":{"target":{"type":"string"}}}}}},
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"mcp__x__test","description":"Test things","input_schema":{"type":"object"}}}},
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"existing","description":"ignored","input_schema":{"type":"string"}}}}
		]}
	],"tools":[
		{"name":"existing","description":"existing tool","input_schema":{"type":"object"}}
	]}`
	req, _ := buildForTest(t, raw)
	assertShapes(t, req.Messages, "user:hi", "system:MCP tools loaded.")
	assertToolsJSON(t, req.Tools, `[{"type":"function","function":{"name":"existing","description":"existing tool","parameters":{"type":"object"}}},{"type":"function","function":{"name":"mcp__x__build","description":"Build things","parameters":{"type":"object","properties":{"target":{"type":"string"}}}}},{"type":"function","function":{"name":"mcp__x__test","description":"Test things","parameters":{"type":"object"}}}]`)
}

// TestBuildOpenAIRequest_DeferredToolAdditionReferenceFormSkipped covers the
// older name-only tool_addition form: without a definition there is nothing
// to add, and the block still drops.
func TestBuildOpenAIRequest_DeferredToolAdditionReferenceFormSkipped(t *testing.T) {
	raw := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"system","content":[
			{"type":"text","text":"loaded"},
			{"type":"tool_addition","tool":{"type":"tool_reference","tool_name":"old_style"}}
		]}
	]}`
	req, _ := buildForTest(t, raw)
	if len(req.Tools) != 0 {
		t.Errorf("tools = %+v, want none", req.Tools)
	}
	assertShapes(t, req.Messages, "system:loaded")
}

// TestBuildOpenAIRequest_DeferredToolAdditionNoSchemaSkipped covers a
// deferred definition without an input schema: it is skipped like a
// schema-less top-level tool.
func TestBuildOpenAIRequest_DeferredToolAdditionNoSchemaSkipped(t *testing.T) {
	raw := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"system","content":[
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"no_schema","description":"skipped"}}}
		]}
	]}`
	req, _ := buildForTest(t, raw)
	if len(req.Tools) != 0 {
		t.Errorf("tools = %+v, want none", req.Tools)
	}
	if len(req.Messages) != 0 {
		t.Errorf("messages = %+v, want none", req.Messages)
	}
}

// TestBuildOpenAIRequest_DeferredDefinitionBeforeTopLevelTools verifies the
// definitions are collected before the converted tools are merged, so a
// definition in messages (listed before the tools member) still appends.
func TestBuildOpenAIRequest_DeferredDefinitionBeforeTopLevelTools(t *testing.T) {
	raw := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"system","content":[
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"late","description":"Late","input_schema":{"type":"object"}}}}
		]}
	],"tools":[{"name":"declared","description":"Declared","input_schema":{"type":"object"}}]}`
	req, _ := buildForTest(t, raw)
	assertToolsJSON(t, req.Tools, `[{"type":"function","function":{"name":"declared","description":"Declared","parameters":{"type":"object"}}},{"type":"function","function":{"name":"late","description":"Late","parameters":{"type":"object"}}}]`)
}

// TestBuildOpenAIRequest_DeferredDuplicateDefinition covers duplicate names
// across block forms: only the first definition is appended.
func TestBuildOpenAIRequest_DeferredDuplicateDefinition(t *testing.T) {
	raw := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"system","content":[
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"dup","description":"first","input_schema":{"type":"object"}}}}
		]},
		{"role":"system","content":[
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"dup","description":"second","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}}}
		]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":[
				{"type":"tool_definition","definition":{"name":"dup","description":"third","input_schema":{"type":"object"}}}
			]}
		]}
	]}`
	req, _ := buildForTest(t, raw)
	assertToolsJSON(t, req.Tools, `[{"type":"function","function":{"name":"dup","description":"first","parameters":{"type":"object"}}}]`)
}

// TestBuildOpenAIRequest_DeferredToolResultDefinition covers rule 2: a
// tool_definition inside tool_result content adds the tool and drops from the
// tool text; a definition-only result keeps an empty tool message to pair
// with the tool_call.
func TestBuildOpenAIRequest_DeferredToolResultDefinition(t *testing.T) {
	t.Run("text and definition", func(t *testing.T) {
		raw := `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"search","input":{"q":"x"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[
					{"type":"text","text":"found it"},
					{"type":"tool_definition","definition":{"name":"mcp__x__read","description":"Read","input_schema":{"type":"object"}}}
				]}
			]}
		]}`
		req, _ := buildForTest(t, raw)
		assertShapes(t, req.Messages, "assistant:tu_1", "tool:tu_1")
		if got := req.Messages[1].Content; got != "found it" {
			t.Errorf("tool content = %v, want %q", got, "found it")
		}
		assertToolsJSON(t, req.Tools, `[{"type":"function","function":{"name":"mcp__x__read","description":"Read","parameters":{"type":"object"}}}]`)
	})

	t.Run("definition only keeps the tool message", func(t *testing.T) {
		raw := `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"search","input":{}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[
					{"type":"tool_definition","definition":{"name":"mcp__x__read","input_schema":{"type":"object"}}}
				]}
			]}
		]}`
		req, _ := buildForTest(t, raw)
		assertShapes(t, req.Messages, "assistant:tu_1", "tool:tu_1")
		if got := req.Messages[1].Content; got != "" {
			t.Errorf("tool content = %v, want empty", got)
		}
		assertToolsJSON(t, req.Tools, `[{"type":"function","function":{"name":"mcp__x__read","parameters":{"type":"object"}}}]`)
	})
}

// TestBuildOpenAIRequest_DeferredBlocksOnlyMessagesDropped covers rules 3 and
// 4: a message whose content held only deferred blocks disappears, while its
// definitions are still collected.
func TestBuildOpenAIRequest_DeferredBlocksOnlyMessagesDropped(t *testing.T) {
	raw := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":[
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"a","input_schema":{"type":"object"}}}},
			{"type":"tool_removal","tool_name":"b"}
		]},
		{"role":"user","content":[
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"c","input_schema":{"type":"object"}}}},
			{"type":"tool_removal","tool_name":"d"}
		]},
		{"role":"assistant","content":[
			{"type":"tool_removal","tool_name":"e"}
		]},
		{"role":"user","content":"done"}
	]}`
	req, _ := buildForTest(t, raw)
	assertShapes(t, req.Messages, "user:hi", "user:done")
	assertToolsJSON(t, req.Tools, `[{"type":"function","function":{"name":"a","parameters":{"type":"object"}}},{"type":"function","function":{"name":"c","parameters":{"type":"object"}}}]`)
}

// TestBuildOpenAIRequest_DeferredRule7Relocation covers rule 7: a system run
// between an assistant's tool_calls and the answering tool messages moves to
// after the last matching tool message, and nowhere else.
func TestBuildOpenAIRequest_DeferredRule7Relocation(t *testing.T) {
	t.Run("single call and one system", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f","input":{}}]},
			{"role":"system","content":"note"},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1", "tool:tu_1", "system:note")
	})

	t.Run("double call and two systems", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"f","input":{}},
				{"type":"tool_use","id":"tu_2","name":"g","input":{}}
			]},
			{"role":"system","content":"one"},
			{"role":"system","content":"two"},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"r1"},
				{"type":"tool_result","tool_use_id":"tu_2","content":"r2"}
			]}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1,tu_2", "tool:tu_1", "tool:tu_2", "system:one", "system:two")
	})

	t.Run("two tool results in one user message", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"f","input":{}},
				{"type":"tool_use","id":"tu_2","name":"g","input":{}}
			]},
			{"role":"system","content":"note"},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"r1"},
				{"type":"tool_result","tool_use_id":"tu_2","content":"r2"}
			]}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1,tu_2", "tool:tu_1", "tool:tu_2", "system:note")
	})

	t.Run("partial tool results still move", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"f","input":{}},
				{"type":"tool_use","id":"tu_2","name":"g","input":{}}
			]},
			{"role":"system","content":"note"},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_2","content":"r2"}
			]}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1,tu_2", "tool:tu_2", "system:note")
	})

	t.Run("no result at the end does not move", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f","input":{}}]},
			{"role":"system","content":"note"}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1", "system:note")
	})

	t.Run("non-tool message breaks the gap and does not move", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f","input":{}}]},
			{"role":"system","content":"note"},
			{"role":"user","content":"next"}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1", "system:note", "user:next")
	})

	t.Run("system after tool messages does not move", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]},
			{"role":"system","content":"note"},
			{"role":"user","content":"next"}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1", "tool:tu_1", "system:note", "user:next")
	})

	t.Run("system elsewhere does not move", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"first"},
			{"role":"system","content":"hook"},
			{"role":"user","content":"second"}
		]}`)
		assertShapes(t, req.Messages, "user:first", "system:hook", "user:second")
	})

	t.Run("synthetic user image stays after the moved system", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"call"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f","input":{}}]},
			{"role":"system","content":"note"},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}
				]}
			]}
		]}`)
		assertShapes(t, req.Messages, "user:call", "assistant:tu_1", "tool:tu_1", "system:note", "user:parts")
	})
}

// TestBuildOpenAIRequest_DeferredGoldenUnchanged covers rule 9: requests
// without deferred blocks and without a rule-7 gap marshal byte-identically.
func TestBuildOpenAIRequest_DeferredGoldenUnchanged(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "no blocks",
			raw:  `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"enable_thinking":false}`,
		},
		{
			name: "with tools",
			raw:  `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"search","description":"Search","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`,
			want: `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"search","description":"Search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],"max_tokens":16,"enable_thinking":false}`,
		},
		{
			name: "assistant tool calls and tool result",
			raw:  `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"weather?"},{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Tokyo","unit":"c"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"sunny"}]}]}`,
			want: `{"model":"glm-5.3","messages":[{"role":"user","content":"weather?"},{"role":"assistant","content":"checking","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\",\"unit\":\"c\"}"}}]},{"role":"tool","content":"sunny","tool_call_id":"tu_1"}],"max_tokens":16,"enable_thinking":false}`,
		},
		{
			name: "system outside the tool gap",
			raw:  `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"system","content":"hook context"},{"role":"user","content":"second"}]}`,
			want: `{"model":"glm-5.3","messages":[{"role":"user","content":"first"},{"role":"system","content":"hook context"},{"role":"user","content":"second"}],"max_tokens":16,"enable_thinking":false}`,
		},
		{
			name: "system after tool results",
			raw:  `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"call"},{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]},{"role":"system","content":"note"},{"role":"user","content":"next"}]}`,
			want: `{"model":"glm-5.3","messages":[{"role":"user","content":"call"},{"role":"assistant","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","content":"ok","tool_call_id":"tu_1"},{"role":"system","content":"note"},{"role":"user","content":"next"}],"max_tokens":16,"enable_thinking":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := buildForTest(t, tc.raw)
			got, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("golden mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
