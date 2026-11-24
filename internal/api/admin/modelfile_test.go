package admin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseModelfile(t *testing.T) {
	mf := "# comment\r\n" +
		"FROM qwen3:8b\n" +
		"PARAMETER temperature 0.6\n" +
		"PARAMETER\tnum_ctx 32768\n" +
		"PARAMETER stop \"<|im_end|>\"\n" +
		"PARAMETER stop <|endoftext|>\n" +
		"SYSTEM \"\"\"You are\n" +
		"a helpful \"bot\".\n" +
		"\"\"\"\n" +
		"TEMPLATE \"\"\"{{ .Prompt }}\"\"\"\n" +
		"MESSAGE user hi\n" +
		"MESSAGE assistant \"hello\"\n" +
		"LICENSE MIT\n"
	got, err := ParseModelfile(mf)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"from":     "qwen3:8b",
		"system":   "You are\na helpful \"bot\".\n",
		"template": "{{ .Prompt }}",
		"license":  "MIT",
		"parameters": map[string]any{
			"temperature": 0.6, "num_ctx": int64(32768), "stop": []string{"<|im_end|>", "<|endoftext|>"},
		},
		"messages": []map[string]string{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}},
	}
	if !reflect.DeepEqual(got, want) {
		a, _ := json.MarshalIndent(got, "", " ")
		t.Errorf("got %s", a)
	}

	for name, bad := range map[string]string{
		"no from":      "SYSTEM hi",
		"local file":   "FROM ./model.gguf",
		"adapter":      "FROM x\nADAPTER ./a.bin",
		"unterminated": "FROM x\nSYSTEM \"\"\"abc\n",
		"unknown":      "FROM x\nFOO bar",
		"bad message":  "FROM x\nMESSAGE tool hi",
		"no param val": "FROM x\nPARAMETER temperature",
	} {
		if _, err := ParseModelfile(bad); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildCreateRequest(t *testing.T) {
	raw := map[string]json.RawMessage{
		"model":     json.RawMessage(`" my-model "`),
		"system":    json.RawMessage(`"explicit"`),
		"template":  json.RawMessage(`""`),
		"modelfile": json.RawMessage(`"FROM base\nSYSTEM from-file\nPARAMETER top_k 20"`),
	}
	got, err := buildCreateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "my-model" || got["from"] != "base" || got["system"] != "explicit" || got["stream"] != true {
		t.Errorf("got %v", got)
	}
	if _, ok := got["template"]; ok {
		t.Error("empty template should be dropped")
	}
	for name, body := range map[string]string{
		"no from":     `{"model":"x"}`,
		"bad name":    `{"model":"a b","from":"y"}`,
		"unknown":     `{"model":"x","from":"y","files":{}}`,
		"bad params":  `{"model":"x","from":"y","parameters":[1]}`,
		"bad mf":      `{"model":"x","modelfile":"FROM ./a.gguf"}`,
		"no name":     `{"from":"y"}`,
		"mf not text": `{"model":"x","modelfile":1}`,
	} {
		var r map[string]json.RawMessage
		if err := json.NewDecoder(strings.NewReader(body)).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if _, err := buildCreateRequest(r); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
