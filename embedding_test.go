package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewWithoutKeyReturnsNil(t *testing.T) {
	e, err := New("https://example.com", "", "some-model")
	if err != nil {
		t.Fatalf("无 key 不应报错: %v", err)
	}
	if e != nil {
		t.Fatal("无 key 应返回 nil（表示未启用语义检索）")
	}
}

func TestEmbedRoundTrip(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"index":     0,
				"embedding": []float64{0.1, 0.2, 0.3},
			}},
		})
	}))
	defer srv.Close()

	e, err := New(srv.URL, "sk-test", "bge-m3")
	if err != nil || e == nil {
		t.Fatalf("构造失败: %v", err)
	}

	vec, err := e.Embed(context.Background(), "你好")
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 || vec[2] != 0.3 {
		t.Fatalf("向量解析不对: %v", vec)
	}
	if e.Dim() != 3 {
		t.Errorf("Dim() 应为 3，得到 %d", e.Dim())
	}
	if gotPath != "/embeddings" {
		t.Errorf("请求路径应为 /embeddings，得到 %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头不对: %s", gotAuth)
	}
	if !contains(gotBody, `"model":"bge-m3"`) || !contains(gotBody, `"input":"你好"`) {
		t.Errorf("请求体不对: %s", gotBody)
	}
}

func TestEmbedAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "invalid api key"},
		})
	}))
	defer srv.Close()

	e, _ := New(srv.URL, "sk-bad", "m")
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("API 错误应当被暴露出来")
	} else if !contains(err.Error(), "invalid api key") {
		t.Errorf("错误信息应带上接口返回: %v", err)
	}
}

func TestEmbedEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	e, _ := New(srv.URL, "sk-test", "m")
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("空 data 应当报错")
	}
}

func TestDefaultBaseURLAndModel(t *testing.T) {
	e, err := New("", "sk-test", "")
	if err != nil {
		t.Fatal(err)
	}
	oe := e.(*openAIEmbedder)
	if oe.baseURL != "https://api.siliconflow.cn/v1" {
		t.Errorf("默认 baseURL 不对: %s", oe.baseURL)
	}
	if oe.model != "BAAI/bge-m3" {
		t.Errorf("默认模型不对: %s", oe.model)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
