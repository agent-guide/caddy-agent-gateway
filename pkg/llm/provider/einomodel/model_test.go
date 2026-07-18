package einomodel

import (
	"context"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type fakeProvider struct {
	lastReq *provider.ChatRequest
	message *schema.Message
}

func (f *fakeProvider) Chat(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	f.lastReq = req
	return &provider.ChatResponse{Message: f.message}, nil
}

func (f *fakeProvider) StreamChat(_ context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	f.lastReq = req
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(f.message, nil)
	sw.Close()
	return sr, nil
}

func (f *fakeProvider) ListModels(_ context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (f *fakeProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{}
}
func (f *fakeProvider) Config() provider.ProviderConfig { return provider.ProviderConfig{} }

func TestGenerateMapsRequestAndReturnsMessage(t *testing.T) {
	fake := &fakeProvider{message: schema.AssistantMessage("hi", nil)}
	m, err := New(fake, "smart")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	out, err := m.Generate(t.Context(), []*schema.Message{schema.UserMessage("hello")}, einomodel.WithTemperature(0.2))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if out.Content != "hi" {
		t.Fatalf("content = %q, want hi", out.Content)
	}
	if fake.lastReq.Model != "smart" {
		t.Fatalf("model = %q, want default route target name", fake.lastReq.Model)
	}
	if len(fake.lastReq.Messages) != 1 || fake.lastReq.Messages[0].Content != "hello" {
		t.Fatalf("messages = %+v, want the input messages", fake.lastReq.Messages)
	}
	common := einomodel.GetCommonOptions(nil, fake.lastReq.Options...)
	if common.Temperature == nil || *common.Temperature != 0.2 {
		t.Fatalf("temperature option lost: %+v", common)
	}
}

func TestPerCallModelOptionOverridesDefault(t *testing.T) {
	fake := &fakeProvider{message: schema.AssistantMessage("ok", nil)}
	m, _ := New(fake, "smart")
	if _, err := m.Generate(t.Context(), []*schema.Message{schema.UserMessage("x")}, einomodel.WithModel("fast")); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if fake.lastReq.Model != "fast" {
		t.Fatalf("model = %q, want per-call override fast", fake.lastReq.Model)
	}
}

func TestWithToolsBindsWithoutMutatingReceiver(t *testing.T) {
	fake := &fakeProvider{message: schema.AssistantMessage("ok", nil)}
	m, _ := New(fake, "smart")
	tools := []*schema.ToolInfo{{Name: "read_file", Desc: "Read a file"}}
	bound, err := m.WithTools(tools)
	if err != nil {
		t.Fatalf("WithTools() error = %v", err)
	}

	if _, err := bound.Generate(t.Context(), []*schema.Message{schema.UserMessage("x")}); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	common := einomodel.GetCommonOptions(nil, fake.lastReq.Options...)
	if len(common.Tools) != 1 || common.Tools[0].Name != "read_file" {
		t.Fatalf("bound tools = %+v, want read_file", common.Tools)
	}

	if _, err := m.Generate(t.Context(), []*schema.Message{schema.UserMessage("x")}); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	common = einomodel.GetCommonOptions(nil, fake.lastReq.Options...)
	if len(common.Tools) != 0 {
		t.Fatalf("original model got tools = %+v, want none (WithTools must not mutate)", common.Tools)
	}

	if _, err := m.WithTools([]*schema.ToolInfo{{Name: " "}}); err == nil {
		t.Fatal("WithTools() with a blank tool name must fail")
	}
}

func TestStreamPassesThrough(t *testing.T) {
	fake := &fakeProvider{message: schema.AssistantMessage("chunk", nil)}
	m, _ := New(fake, "smart")
	sr, err := m.Stream(t.Context(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer sr.Close()
	msg, err := sr.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if msg.Content != "chunk" {
		t.Fatalf("chunk = %q, want chunk", msg.Content)
	}
}
