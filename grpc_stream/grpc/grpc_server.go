package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	chatpb "chatGPT_streaming/grpc_stream/protos"

	"github.com/rs/zerolog/log"
	goopenai "github.com/sashabaranov/go-openai"
	"google.golang.org/grpc"
)

type OpenaiService struct {
	chatpb.UnimplementedOpenaiServiceServer
}

const (
	// Address 监听地址
	Address string = ":50050"
	// Network 网络通信协议
	Network string = "tcp"
	// Timeout 服务端超时时间
	Timeout int = 5
)

type MyChannel struct {
	C    chan bool
	once sync.Once
}

func NewMyChannel() *MyChannel {
	return &MyChannel{C: make(chan bool)}
}

// SafeClose 保证chan只被关闭一次，避免业务中混乱的close,导致被关闭的chan再close触发panic
func (mc *MyChannel) SafeClose() {
	mc.once.Do(func() {
		close(mc.C)
	})
}

func main() {
	// 监听本地端口
	listener, err := net.Listen(Network, Address)
	if err != nil {
		log.Fatal().Msgf("net.Listen err: %v", err)
	}
	log.Log().Msg(Address + " net.Listing...")
	// 新建gRPC服务器实例
	grpcServer := grpc.NewServer()
	// 在gRPC服务器注册我们的服务
	chatpb.RegisterOpenaiServiceServer(grpcServer, &OpenaiService{})
	// 用服务器 Serve() 方法以及我们的端口信息区实现阻塞等待，直到进程被杀死或者 Stop() 被调用
	err = grpcServer.Serve(listener)
	if err != nil {
		log.Fatal().Msgf("grpcServer.Serve err: %v", err)
	}
}

const (
	BaseURL = "https://api.openai.com/v1"
	Token   = ""
)

func (s *OpenaiService) StreamChatCompletion(request *chatpb.StreamChatCompletionRequest, completionServer chatpb.OpenaiService_StreamChatCompletionServer) error {
	// 鉴权 - 自己增加

	// 业务
	var messages []goopenai.ChatCompletionMessage
	for _, v := range request.Message {
		messages = append(messages, goopenai.ChatCompletionMessage{
			Role:    v.Role,
			Content: v.Content,
		})
	}

	log.Debug().Msgf("messages: %v", messages)
	// 默认openai的token
	if request.GetToken() == "" {
		request.Token = Token
	}
	config := goopenai.DefaultConfig(request.Token)
	config.BaseURL = BaseURL
	// proxyUrl, err := url.Parse("http://127.0.0.1:10887")
	// if err != nil {
	// 	log.Fatal().Msgf("failed to init proxy: %v", err)
	// 	return err
	// }
	// http.DefaultTransport = &http.Transport{Proxy: http.ProxyURL(proxyUrl)}
	// config.HTTPClient.Transport = http.DefaultTransport
	client := goopenai.NewClientWithConfig(config)
	req := goopenai.ChatCompletionRequest{
		Model:    goopenai.GPT3Dot5Turbo,
		Messages: messages,
		Stream:   true,
	}
	ctx := context.Background()
	stream, err := client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		log.Err(err).Msgf("ChatCompletionStream error: %v\n", err)
		return err
	}
	defer stream.Close()

	log.Log().Msgf("Stream response: ")

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			log.Log().Msgf("Stream finished: %v\n", err)
			return nil
		}
		if err != nil {
			log.Err(err).Msgf("Stream error: %v\n", err)
			return err
		}
		res := chatpb.StreamChatCompletionResponse{
			Content: response.Choices[0].Delta.Content,
		}
		if err := completionServer.Send(&res); err != nil {
			log.Err(err).Msgf("error while sending message to stream: %v\n", err)
			return err
		}
	}
}
