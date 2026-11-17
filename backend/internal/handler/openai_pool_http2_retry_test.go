//go:build unit

package handler

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type poolPeerResetHTTPUpstream struct {
	peerResetHTTPUpstream
	accountIDs []int64
	bodies     []string
	onResponse func(*http.Response)
}

func (u *poolPeerResetHTTPUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.accountIDs = append(u.accountIDs, accountID)
	u.bodies = append(u.bodies, string(body))
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	resp, err := u.peerResetHTTPUpstream.Do(req, proxy, accountID, concurrency)
	if resp != nil && u.onResponse != nil {
		u.onResponse(resp)
	}
	return resp, err
}

func TestOpenAIPoolHTTP2PeerResetRetriesSameAccount(t *testing.T) {
	testOpenAIPoolStreamResetRetriesSameAccount(t, "http2")
}

func TestOpenAIPoolTCPReadResetRetriesSameAccount(t *testing.T) {
	testOpenAIPoolStreamResetRetriesSameAccount(t, "tcp")
}

func testOpenAIPoolStreamResetRetriesSameAccount(t *testing.T, transport string) {
	for _, mode := range []string{"responses", "passthrough", "responses_via_chat", "chat_via_responses", "raw_chat", "messages", "messages_via_chat"} {
		for _, partial := range []bool{false, true} {
			for _, recover := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/partial=%v/recover=%v", mode, partial, recover), func(t *testing.T) {
					setupOpsErrorLogTestQueue(t, 8)
					var submissions atomic.Int32
					resetReady := make(chan struct{}, 1)
					server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if transport == "http2" && r.ProtoMajor != 2 {
							t.Error("fixture must negotiate real HTTP/2")
						}
						_, _ = io.Copy(io.Discard, r.Body)
						attempt := submissions.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						if !recover || attempt == 1 {
							if transport == "tcp" {
								payload := ""
								if partial {
									if strings.HasSuffix(r.URL.Path, "/chat/completions") {
										payload = "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
									} else {
										payload = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
									}
								}
								writePoolTCPReset(t, w, payload, resetReady)
								return
							}
							if partial {
								if strings.HasSuffix(r.URL.Path, "/chat/completions") {
									_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
								} else {
									_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
								}
							}
							w.(http.Flusher).Flush()
							panic(http.ErrAbortHandler)
						}
						if strings.HasSuffix(r.URL.Path, "/chat/completions") {
							_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"recovered\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
						} else {
							_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
						}
					}))
					if transport == "http2" {
						server.EnableHTTP2 = true
						server.StartTLS()
					} else {
						server.Start()
					}
					defer server.Close()
					base, err := url.Parse(server.URL)
					require.NoError(t, err)
					upstream := &poolPeerResetHTTPUpstream{peerResetHTTPUpstream: peerResetHTTPUpstream{client: server.Client(), base: base}}
					if transport == "tcp" {
						upstream.onResponse = func(resp *http.Response) {
							if partial {
								resp.Body = &poolTCPObservedBody{ReadCloser: resp.Body, ready: resetReady}
							} else {
								resetReady <- struct{}{}
							}
						}
					}
					extra := map[string]any{}
					if mode == "passthrough" {
						extra["openai_passthrough"] = true
					}
					if mode == "responses_via_chat" || mode == "raw_chat" || mode == "messages_via_chat" {
						extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceChatCompletions)
					}
					account := service.Account{ID: 1, Name: "synthetic", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid", "pool_mode": true, "pool_mode_retry_count": 2, "pool_mode_retry_status_codes": []any{502}}, Extra: extra}
					other := account
					other.ID, other.Priority = 2, 10
					h := newHTTP2PeerResetHandler(t, upstream, account, other)
					repo := &ingressRejectOpsRepo{}
					ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
					endpoint, body, handler := "/v1/responses", `{"model":"gpt-5.1","stream":true,"input":"synthetic"}`, h.Responses
					if mode == "raw_chat" || mode == "chat_via_responses" {
						endpoint, body, handler = "/v1/chat/completions", `{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"synthetic"}]}`, h.ChatCompletions
					} else if strings.HasPrefix(mode, "messages") {
						endpoint, body, handler = "/v1/messages", `{"model":"gpt-5.1","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"synthetic"}]}`, h.Messages
					}
					router := gin.New()
					router.Use(OpsErrorLoggerMiddleware(ops))
					var terminalFailure bool
					router.POST(endpoint, func(c *gin.Context) {
						seed, _ := newOpenAIResponsesFailoverTestContext(t, c.Request.Context())
						for k, v := range seed.Keys {
							c.Set(k, v)
						}
						key, _ := middleware.GetAPIKeyFromContext(c)
						key.Group.AllowMessagesDispatch = true
						handler(c)
						_, terminalFailure = service.GetOpsStreamError(c)
					})
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body)))
					for len(opsErrorLogQueue) > 0 {
						flushOpsErrorLogBatch([]opsErrorLogJob{<-opsErrorLogQueue})
					}
					attempts := 3
					if recover {
						attempts = 2
					}
					require.Len(t, upstream.accountIDs, attempts, rec.Body.String())
					for i, id := range upstream.accountIDs {
						require.Equal(t, int64(1), id, "retry must stay on the original account")
						require.Equal(t, upstream.bodies[0], upstream.bodies[i], "retry must preserve the exact request body")
					}
					if recover {
						require.Equal(t, 200, rec.Code)
						require.Contains(t, rec.Body.String(), "recovered")
						require.NotContains(t, rec.Body.String(), "upstream_http2_stream_error")
						require.NotContains(t, rec.Body.String(), "upstream_stream_read_error")
						require.False(t, terminalFailure, "recovered intermediate reset must not mark the final request failed")
						require.Len(t, repo.entries, 1, "retain the existing recovered-upstream diagnostic row")
						require.Equal(t, 200, repo.entries[0].StatusCode)
						require.Contains(t, repo.entries[0].ErrorMessage, "Recovered upstream error")
						require.Nil(t, repo.entries[0].UpstreamStatusCode)
					} else {
						require.Len(t, repo.entries, 1, rec.Body.String())
						require.Equal(t, "transport_error", repo.entries[0].ErrorType)
						require.Nil(t, repo.entries[0].UpstreamStatusCode, "effective retry status 502 is not a real HTTP status")
						code := service.OpenAIUpstreamHTTP2StreamErrorCode
						if transport == "tcp" {
							code = service.OpenAIUpstreamStreamReadErrorCode
						}
						require.Equal(t, 1, strings.Count(rec.Body.String(), "\"code\":\""+code+"\""))
						if transport == "tcp" {
							require.Contains(t, repo.entries[0].ErrorBody, `"protocol":"TCP"`)
							require.Contains(t, repo.entries[0].ErrorBody, "CONNECTION_RESET")
							require.NotContains(t, rec.Body.String(), "127.0.0.1")
						}
						require.NotContains(t, rec.Body.String(), "[DONE]")
						require.NotContains(t, rec.Body.String(), "response.completed")
					}
					if partial {
						require.Contains(t, rec.Body.String(), "partial")
					}
				})
			}
		}
	}
}

type poolTCPObservedBody struct {
	io.ReadCloser
	ready chan<- struct{}
	once  sync.Once
}

func (b *poolTCPObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.once.Do(func() { b.ready <- struct{}{} })
	}
	return n, err
}

// Wait for the client to consume headers (or a partial body) before sending a
// real TCP RST. This avoids timing-dependent EOF/reset behavior in the fixture.
func writePoolTCPReset(t *testing.T, w http.ResponseWriter, payload string, ready <-chan struct{}) {
	t.Helper()
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Error("fixture requires a real TCP connection")
		return
	}
	if err := tcp.SetLinger(0); err != nil {
		t.Error(err)
		return
	}
	_, err = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
	if err == nil && payload != "" {
		_, err = fmt.Fprintf(rw, "%x\r\n%s\r\n", len(payload), payload)
	}
	if err == nil {
		err = rw.Flush()
	}
	if err != nil {
		t.Error(err)
		return
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Error("client did not consume the reset fixture response")
	}
}
