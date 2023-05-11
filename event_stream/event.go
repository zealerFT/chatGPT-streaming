package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang/glog"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	goopenai "github.com/sashabaranov/go-openai"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type wsControl struct {
	Message []Message
}

type Message struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}

type Request struct {
	Type    string    `json:"type"`
	Message []Message `json:"message"`
}

// 自定义基础配置
const (
	BaseURL = "https://api.openai.com/v1"
	Token   = ""
)

func ChatGPTStream(c *gin.Context) {
	var syncOnce sync.Once
	// Upgrade the HTTP connection to a WebSocket connectio
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Fatal().Msgf("failed to upgrader.Upgrade ws: %v", err)
		return
	}

	logs := log.With().Str("component", "ChatGPTStream").Logger()

	logs.Log().Msg("Ws is real begining!")

	// 初始化openai client(github.com/sashabaranov/go-openai)
	config := goopenai.DefaultConfig(Token)
	config.BaseURL = BaseURL
	// 这里可以设置代理，本地调试更加方便，线上部署建议直接在海外服务器，更加稳定（非大中华区不用考虑）
	// proxyUrl, err := url.Parse("http://127.0.0.1:10887")
	// if err != nil {
	// 	log.Fatal().Msgf("failed to init proxy: %v", err)
	// 	return
	// }
	// http.DefaultTransport = &http.Transport{Proxy: http.ProxyURL(proxyUrl)}
	// config.HTTPClient.Transport = http.DefaultTransport
	client := goopenai.NewClientWithConfig(config)

	// channel list
	closeStream := make(chan struct{})
	goroutineClose := make(chan struct{})
	clientCloseWs := make(chan struct{})
	serverCloseWs := make(chan struct{})
	messageWs := make(chan *wsControl)
	grpcStream := make(chan struct{})
	authCheckChan := make(chan struct{})

	var req []goopenai.ChatCompletionMessage

	// 发送ws消息给前端
	sendWsMessage := func(content string, errs ...string) {
		if content == "auth error" || content == "stream error" || len(errs) > 0 {
			err = ws.WriteJSON(gin.H{"type": "stream_chat_ack", "content": content, "error": errs[0]})
			if err != nil {
				log.Log().Msgf("stream_chat_ack Failed to write message: %v", err)
				return
			}
			close(authCheckChan)
			return
		} else {
			err = ws.WriteJSON(gin.H{"type": "stream_chat_ack", "content": content, "error": ""})
			if err != nil {
				log.Log().Msgf("stream_chat_ack Failed to write message: %v", err)
				return
			}
		}
	}

	// 处理streaming
	StreamChatCompletion := func(stream *goopenai.ChatCompletionStream, closeStream chan struct{}) {
		log.Log().Msgf("begin new chat stream")
		var content string
		defer func() {
			log.Log().Msgf("current chat stream end :%s", content)
		}()
		for {
			select {
			case <-closeStream:
				return
			default:
				response, err := stream.Recv()
				if err != nil {
					if err == io.EOF {
						// 完成读取
						sendWsMessage("io.EOF")
						// 自动上下文
						req = append(req, goopenai.ChatCompletionMessage{
							Role:    goopenai.ChatMessageRoleUser,
							Content: content,
						})
					} else {
						// 出现意外错误的情况
						log.Err(err).Msgf("StreamChatCompletion An error occurred midway %v", err)
						sendWsMessage("stream error", "服务繁忙，请稍后重试！")
						close(grpcStream)
					}
					return
				} else {
					if response.Choices[0].Delta.Content == "" {
						// 特殊处理
						continue
					}
					// 发送数据到客户端
					content = content + response.Choices[0].Delta.Content
					sendWsMessage(response.Choices[0].Delta.Content)
				}
			}
		}
	}

	defer func(wait func()) {
		select {
		case <-closeStream:
			log.Info().Msg("ending ChatGPTStream")
			wait()
			log.Info().Msg("ChatGPTStream ended")
		default:
			log.Error().Msg("something terrible happened")
		}
	}(goAndWait(c, goroutineClose,
		func() {
			log.Info().Msg("close pipe start")
			defer log.Info().Msg("close pipe end")
			defer close(closeStream)
			select {
			case <-clientCloseWs:
			case <-grpcStream:
			case <-goroutineClose:
			case <-serverCloseWs:
			case <-authCheckChan:
			}
		},
		func() {
			// auth,可以这样添加一个新协程来处理自己的业务，比如检查用户的权限，避免被刷爆，然后close(authCheckChan)来处理
		},
		func() {
			log.Info().Msg("healthcheck-server start")
			defer log.Info().Msg("healthcheck-server end")
			ticker := time.NewTicker(time.Second * 5)
			for {
				select {
				case <-closeStream:
					return
				case <-ticker.C:
					if err := ws.WriteJSON(gin.H{
						"type": "healthcheck-server",
					}); err != nil {
						log.Error().Err(err).Caller(1).Msg("failed to write healthcheck-server to websocket")
						syncOnce.Do(func() {
							close(clientCloseWs)
						})
					}
				}
			}
		},
		func() {
			defer func() {
				log.Log().Msg("StreamChatCompletion close!")
			}()
			for {
				time.Sleep(100 * time.Millisecond)
				select {
				case <-closeStream:
					return
				case message := <-messageWs:
					for _, v := range message.Message {
						req = append(req, goopenai.ChatCompletionMessage{
							Role:    v.Role,
							Content: v.Content,
						})
					}
					log.Info().Msgf("ChatCompletionMessage info: ", req)
					stream, err := client.CreateChatCompletionStream(c, goopenai.ChatCompletionRequest{
						Model:    goopenai.GPT3Dot5Turbo,
						Messages: req,
						Stream:   true,
					})
					if err != nil {
						log.Err(err).Msgf("StreamChatCompletion fail %v", err)
						sendWsMessage("grpc stream error", "服务繁忙，请稍后重试！")
						close(grpcStream)
						return
					}
					StreamChatCompletion(stream, closeStream)
				default:
				}
			}
		},
	))

	defer func() {
		time.Sleep(time.Millisecond * 500) // 协程的close可能还未来得及执行
		select {
		case <-clientCloseWs:
			log.Log().Msg("clientCloseWs: ws closing connection")
			return
		case <-serverCloseWs:
			log.Log().Msg("serverCloseWs: ws closing connection")
			return
		default:
			log.Info().Msg("chat stream closing websocket")
			err := ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
			if err != nil {
				log.Error().Err(err).Caller().Msg("failed to send close message")
			}
			err = ws.Close()
			if err != nil {
				log.Error().Err(err).Caller().Msg("failed to close websocket")
			}
		}
	}()

	// js close
	websocketNormalClose := func(err error) bool {
		for _, v := range []int{websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived} {
			if websocket.IsCloseError(err, v) {
				log.Info().Msgf("normal closing connection code: %v", v)
				syncOnce.Do(func() {
					close(clientCloseWs)
				})
				return true
			}
		}
		return false
	}

	for {
		select {
		case <-closeStream:
			log.Log().Msg("ws closing connection")
			return
		default:
			if err := ws.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				log.Err(err).Caller().Msg("failed to set read deadline")
				break
			} else if mt, message, err := ws.ReadMessage(); err != nil {
				if !websocketNormalClose(err) {
					log.Err(err).Msg("failed to read message, closing connection")
					close(serverCloseWs)
				} else {
					time.Sleep(time.Millisecond * 500) // 避免下次循环closeStreamh还未close,导致进入default
				}
				return
			} else {
				switch mt {
				case websocket.CloseMessage:
					log.Info().Msg("got close message, closing connection")
					syncOnce.Do(func() {
						close(clientCloseWs)
					})
				case websocket.TextMessage:
					log.Info().Msgf("got ws message %s", message)
					msg := Request{}
					if err := json.Unmarshal(message, &msg); err != nil {
						log.Err(err).Msg("failed to unmarshal text message")
						close(serverCloseWs)
						return
					}
					if msg.Type == "healthcheck-client" || msg.Type == "healthcheck-server" || msg.Type == "healthcheck" {
						if err := ws.WriteJSON(msg); err != nil {
							log.Error().Err(err).Caller(1).Msg("failed to write healthcheck to websocket")
						}
						continue
					} else if msg.Type == "stream_chat" {
						messageWs <- &wsControl{
							Message: msg.Message,
						}
					} else {
						log.Error().Msg("unknown message")
						close(serverCloseWs)
						return
					}
				default:
					log.Log().Msgf("success get ws default %v", mt)
				}
			}
		}
	}
}

func main() {
	r := gin.Default()
	r.Use(
		func(c *gin.Context) {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
			c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")
			if c.Request.Method == "OPTIONS" {
				c.AbortWithStatus(204)
			}
		},
	)
	r.GET("/stream", ChatGPTStream)
	server := &http.Server{Addr: ":8080", Handler: r}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			glog.Fatalf("server failure: %v", err)
		}
	}()

	log.Log().Msg("Ws is begining :)")

	termination := make(chan os.Signal)
	signal.Notify(termination, syscall.SIGINT, syscall.SIGTERM)
	<-termination

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		glog.Fatalf("Failed to shut down: %v", err)
	}

	log.Log().Msg("Ws shutting down :)")

}

func goAndWait(c *gin.Context, goroutineClose chan struct{}, fns ...func()) func() {
	var wg sync.WaitGroup
	wg.Add(len(fns))
	var once sync.Once
	for _, fn := range fns {
		go func(fn func()) {
			defer func() {
				if err := recover(); err != nil {
					// 记录日志
					log.Err(err.(error)).Msgf("stack: %s", debug.Stack())
					closeFn := func() {
						close(goroutineClose)
					}
					once.Do(closeFn)
				}
			}()
			defer wg.Done()
			fn()
		}(fn)
	}
	return wg.Wait
}
