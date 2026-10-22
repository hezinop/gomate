// Package embedding 提供 OpenAI 兼容的文本向量化客户端。
//
// 为什么不用 eino-ext 的 embedding 组件：eino-ext 稳定版还没有这个组件，
// 而 OpenAI 兼容的 /embeddings 协议本身极简（一个 POST、一个数组），
// 自己写约 80 行，不引入新依赖，也不被组件版本演进绑住。
//
// 默认指向 SiliconFlow（硅基流动）：国内可直连、注册送免费额度、
// OpenAI 兼容。没有配 Key 时整个语义检索自动降级为纯 FTS 关键词检索
// （见 memostore.WithEmbedder 的约定）。
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Embedder 把文本转成向量。
// 抽象成接口：memostore 的混合检索只依赖它，测试时可整体替换。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dim() int
}

// openAIEmbedder 是 OpenAI 兼容 /embeddings 的实现。
type openAIEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	dim     int
	client  *http.Client
}

// New 构造 embedding 客户端。
//
// apiKey 为空时返回 (nil, nil) —— 表示「未启用语义检索」，
// 调用方据此走纯关键词降级，而不是报错。
func New(baseURL, apiKey, model string) (Embedder, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, nil
	}
	if baseURL == "" {
		baseURL = "https://api.siliconflow.cn/v1"
	}
	if model == "" {
		model = "BAAI/bge-m3"
	}
	return &openAIEmbedder{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

type embedRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	EncodingFormat string `json:"encoding_format"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Embed 把单条文本转成向量。
func (e *openAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{
		Model:          e.model,
		Input:          text,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 embedding 接口失败: %w", err)
	}
	defer resp.Body.Close()

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("解析 embedding 响应失败 (HTTP %d): %w", resp.StatusCode, err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("embedding 接口错误: %s", er.Error.Message)
	}
	if len(er.Data) == 0 || len(er.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding 接口返回为空 (HTTP %d)", resp.StatusCode)
	}

	// JSON 数字统一按 float64 解出，再转 float32 存储
	vec := make([]float32, len(er.Data[0].Embedding))
	for i, v := range er.Data[0].Embedding {
		vec[i] = float32(v)
	}
	e.dim = len(vec)
	return vec, nil
}

// Dim 返回最近一次向量的维度（0 表示还没调用过）。
func (e *openAIEmbedder) Dim() int { return e.dim }
