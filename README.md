# chatGPT_streaming
go语言使用openai的ChatGPT接口实践，使用流式传输，类似ChatGPT网页效果，并且可以让ChatGPT的服务单独部署（单独部署到非大中华区的服务器），并用grpc streaming
做中间层，保证不受墙的影响。使用websocket最终通信，后端考虑多种情况，健壮可用。
非常方便移植到你自己的项目，赶紧来看看吧！

## 演示
![日志](https://github.com/zealerFT/chatGPT_streaming/blob/main/source/demo.png)
![GIF演示](https://github.com/zealerFT/chatGPT_streaming/blob/main/source/demo.gif)
## 前置知识
- http text/event-stream
- Streaming with gRPC
- websocket
- go routine and channel

## event_stream
访问后端使用的是text/event-stream
配置：
- BaseURL默认是：https://api.openai.com/v1
- token: 使用自己注册的openai的token，注意不要泄漏自己的token
- 可以选择代理请求，方便本地测试，详细逻辑见具体代码
  ```golang
    proxyUrl, err := url.Parse("http://127.0.0.1:10887")
    if err != nil {
        log.Fatal().Msgf("failed to init proxy: %v", err)
        return
    }
    http.DefaultTransport = &http.Transport{Proxy: http.ProxyURL(proxyUrl)}
    config.HTTPClient.Transport = http.DefaultTransport
    client := goopenai.NewClientWithConfig(config)
  ```
启动：
- 直接执行main()函数
- 然后打开ws_demo.html
- 愉快的对话了，注意我使用了context上下文传递，超过4090token数量会接口抛错，具体逻辑请自行修改
