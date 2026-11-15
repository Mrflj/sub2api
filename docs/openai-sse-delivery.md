# OpenAI 流式首事件与内容计时

OpenAI 流式使用记录区分首事件、首语义、首推理和首文字。首个事件可能只是 `response.created`，不代表模型已经开始输出正文。已有 `first_token_ms`、TPS 和计费口径保留，历史记录不回算。

## 查看与计时范围

启用请求耗时详情后，点击使用记录的延迟单元格，在详情的最终出口内容时序中查看：

| 字段 | 含义 |
| --- | --- |
| `sse_event_first_ms` | 宿主写出并 flush 首个完整、非心跳 SSE data 事件 |
| `semantic_first_ms` | 现有共享判定器识别到首个语义进展 |
| `reasoning_first_ms` | 首个明确推理事件 |
| `visible_text_first_ms` | 首个可见正文文字 |

时间均相对应用入口，属于最终出口 attempt。未知显示 `—`，不能从全局事件或前一失败 attempt 补值。HTTP、可信插件和原生 WS 的采集来源分别标注；逻辑 WS 响应有独立生命周期，不使用连接握手或写请求的耗时替代内容时序。旧记录未包含最终出口标记时保留旧采集范围，缺失项继续未知。

flush 只能证明应用侧写出，不能证明远端客户端已经收到。首事件早而正文晚，应结合上游生成时间和代理缓冲逐段检查。插件 gRPC 响应头和原生 WS 握手不作为远端 TCP 首字节证据，相应 `response_byte_first_ms` 保持缺失。

原生 OAuth WS→SSE 加速沿用现有账号开关、连接池和安全 fallback：只有可证明未提交请求的失败才允许切换传输，不能把“尚未收到首事件”解释为“上游没有执行”。新增观测不改变选路或自动启用加速。

## 可选的可信插件元数据提前交付

默认关闭。YAML：

```yaml
gateway:
  openai_plugin_sse_early_events_enabled: false
```

环境变量为 `GATEWAY_OPENAI_PLUGIN_SSE_EARLY_EVENTS_ENABLED`。Compose 示例已显式映射，改为 `true` 后重建应用容器；二进制部署修改启动环境/YAML 后按既有方式重启服务。它是宿主启动配置，不是插件页面热配置。

开启后还需同时满足：

- 账号为 OpenAI OAuth，请求实际选中出站插件 `local.sub2api.openai-ws-sse-accelerator`。
- 插件兼容当前宿主、成功启用，安装签名状态为 `trusted`。
- 响应为 HTTP 200、`text/event-stream`，并声明 `X-Sub2api-Plugin-Preflight-Sse: 1`。
- 宿主 PluginManager 在成功调度边界赋予私有 context 来源标记；普通上游伪造同名响应头不能获得能力。

API Key 池模式不具备该插件元数据提前交付资格。OpenAI 池账号开启同账号重试且至少配置一个错误状态码时，仅暂存尚未交付的前导元数据和不透明状态；每个完整事件先按协议结构判错，确认是有效正文、思考或工具事件后立即释放并继续流式转发，不缓存整轮正常回答，也不等待成功终态。首次有效输出前的 HTTP 或流内可重试错误按语义状态码和账号配置的次数重试，失败前导不会下发；仅配置 429 的账号也适用。开启插件提前交付开关不会改变池账号的适用范围，首事件交付时间只记录实际 flush 的时点。

完整空行边界闭合的 `response.created/queued/in_progress/metadata` 可提前写出并 flush；未知事件、心跳、`[DONE]` 和不完整事件不会因这个开关获得新资格。元数据提交不解除语义首输出期限，也不伪造首语义或正文。

该插件响应提前提交后，后续事件也在完整边界闭合后交付，避免将半个上游事件与本地错误拼接。每个暂存事件上限为 8 MiB，超过后结束当前流，不自动重放；正常语义输出后同样适用。需要单事件大于此上限的插件应保持此开关关闭，不能把多个 data 行用作绕过限额的方式。

响应提前提交后，故障切换窗口会收窄。后续超时、EOF 或读取失败在当前 SSE 内结束，不跨账号重放或拼接另一条响应；客户端断连沿用既有用量收集。这是交付行为变化，启用前应在隔离候选实例验收两条宿主 SSE parser。

本功能不附带第三方插件包或发布者公钥，不放宽兼容版本与签名校验。研究时收到的 v0.2.2 包 manifest 为 `>=0.2.4 <0.3.0`，不能直接用于 2.x 宿主；修改 manifest 会破坏签名，需要发布者交付兼容范围正确的有效签名包。`allow_unsigned` 不会使签名状态变为 `trusted`。

## 池账号的大响应重试额度

仅 OpenAI 池账号在重试次数大于零且重试状态码列表包含错误码时使用独立额度；未设置列表沿用账号默认码，显式空列表关闭该策略。`gateway.openai_stream_retry_max_line_bytes` 默认 32 MiB，控制原生 Responses 首个语义输出前的单行保护；`gateway.openai_stream_retry_max_buffer_bytes` 默认 64 MiB，控制 Responses、透传、Messages 和 Chat 转换路径首次有效输出前的暂存。正常输出开始后释放前导缓冲并继续流式发送，缓冲额度不作为整轮回答的大小上限。对应环境变量见部署示例，零表示默认值，允许范围分别为 1–128 MiB 和 1–512 MiB。全局 `gateway.max_line_size` 继续生效。

复用既有暂存实现：Linux 超过 64 KiB 后使用临时文件，Windows 沿用有总量上限的内存回退。其他账号、OAuth 插件及语义输出后的读取方式保持原策略。首次有效输出前失败则丢弃该次前导数据；大小错误按账号配置有界重试，不消耗首输出超时的独立恢复次数。有效输出交付后发生错误，在当前协议流中明确结束，不拼接重试生成的另一份回答。取消、断连或已交付实际内容的请求不得重放。调整额度是启动配置，无数据库迁移。

错误事件与有效输出分别判定：`error`、`response.error`、`response.failed`、顶层非空 `error`、嵌套 `response.error` 和失败状态不作为正文、首字或工具输出，即使同一帧声称自己是文本增量。不可重试错误由专门的失败分支返回，不能为“错误不是正文”而隐藏错误。模型正常文字或工具参数中的 `error` 字段不受影响。

逐事件识别无法预知后续事件：如果上游先返回正常内容并已发送给客户端，之后才报错，本次请求按原协议返回错误并结束，不再重试生成另一份回答，避免重复正文或工具调用。要保证这种晚到错误也能完全隐藏并重试，必须缓冲整轮响应，与正常响应即时发送不能同时保证。

## 验收与回滚

CI 使用受控时钟和 mock 验证 created 100ms、推理 300ms、正文 800ms 的独立计时，以及最终 attempt 隔离、分片/多行/半事件、EOF/读取错误/超时、伪造来源、断连和单次终态。该覆盖不等于真实插件或生产链路验收。实际启用后需另行测量直连、域名与真实下游的完整事件和正文到达时间；反代仍需允许流式交付。

将宿主开关改回 `false` 并按既有发布流程重启，可恢复插件原暂存策略，观测继续保留。本功能沿用 timing JSON，无数据库迁移；完整二进制回滚无需恢复历史 timing 数据。在途 SSE 按现有发布方式排空。

## Messages 工具执行语义与流转换

`/v1/messages` 转发到 Responses 或 Chat 时，`tool_choice.disable_parallel_tool_use: true` 对应 `parallel_tool_calls: false`；未提供该字段或明确为 `false` 时，保留原来的并行默认值。`auto`、`any` 和指定工具的选择方式不改变，原生 Anthropic 转发也不为这些兼容规则重建请求。

Messages 的 `tool_result.is_error` 在 OpenAI function result 中没有原生对应字段。因此只有失败结果才编码为以下结果数据，成功文本保持原样，普通文字中的 `error` 不会被识别成执行失败：

```json
{"is_error":true,"content":"原始工具结果文本"}
```

仅包含这两个字段、布尔值为真且 `content` 为字符串的已有相同封装不会再次包装；其他 JSON 仍作为原始结果文本保留。图片沿用现有分离传递方式，作为后续 user image 交给模型，调用与结果的关联 ID 不变。该封装表达的是工具返回数据，不代表网关执行了工具，也不会触发工具自动重放。

Responses 的并行参数按 `output_index` 隔离：首个工具仍即时流式，后续工具等待前一个真正完成后再打开自己的 Messages block。参数 done、item done 或终端快照结束对应调用；完整快照只补确定缺失的参数后缀。Read 的空 `pages` 处理继续保留。真正的 `custom_tool_call` 自由文本按照已有 custom 降级 schema 编码为 `{"input":"原始自由文本"}`；流式片段在字符串内部逐段 JSON 转义，done 和快照先在原始文本层补尾再完成对象。已经是 function 型的 `{"input":...}` 参数不重复包装。

Chat 没有逐个工具的 done 事件，因此首个工具持续流式，其他调用在 `finish_reason` 后依次输出。工具后的正文若与尚未完成的工具交错，会等该工具完成后再输出；常规正文首字不等待终态。随后到达的 usage 仍归入唯一终端事件。已完成调用的迟到事件不会关闭新调用，`message_stop` 后不再输出内容。

Messages/Responses 的 Chat 上游回退共用业务终态校验：每个实际出现的 choice 必须具有支持的非空 `finish_reason`。已有合法终态和后续 usage 时允许直接 EOF，兼容未发 `[DONE]` 的服务；只有 `[DONE]`、只有 role/空正文、或工具参数已经是合法 JSON 但没有终态，都不能合成成功结束。`error`、`response.failed`、顶层 error envelope 和仅由 SSE `event: error` 标识的失败在转换前识别，错误自带的 usage 可保留，但不算模型内容。

角色和空正文等前导事件使用有界私有暂存，首段实际输出立即释放；未输出前的可重试失败交给现有账号/池重试规则。已经输出后，失败只回写当前端点的错误事件，不重放、不发成功终止事件，也不将已有部分工具参数发布为正常完成。取消后不释放暂存内容。明确的 HTTP/2 reset 继续使用已有独立分类，不因错误文本伪造 transport evidence。

回归测试既检查 delta 对应的 block 曾经存在，也检查发送时仍然打开，并核对每个调用的完整参数、ID、失败标志、图片、usage 和单次终止。测试使用本地数据及模拟上游，不调用真实模型或 MCP 服务。

## 请求媒体与生成参数的跨协议边界

原始同协议转发继续保留请求体及供应商扩展字段；以下转换规则只用于真正进入 apicompat 的跨协议请求，不据此替换模型或宣称模型具有某种媒体能力。模型、账号或上游本身不支持某种媒体时，仍由其原生端点验证。

- Messages 的 base64 PDF document 转为 Responses `input_file.file_data` 或 Chat `file.file_data`；PDF 数据、文件名、URL 图片及工具结果中的 PDF/图片会保留。Messages URL document 可转为 Responses `file_url`，Chat 无对应文件 URL 字段时明确返回 400，不自动下载文件。
- Responses `input_file` 的 `file_data` / `file_id` 转为 Chat `file`；向 Messages 转换只接受 PDF data URI 或 HTTP(S) URL。不同供应商的文件 ID 不可直接复用，无法转换时返回 400，不能把文档所在消息静默丢掉。
- 图片 URL 在 Messages/Responses/Chat 间保留；Chat 与 Responses 之间同时保留 `detail`。Responses 的 `original` 在 Chat 目标中采用其可表达的 `high` 提示，图像 URL/数据不变；原生 Responses 的媒体提取仍保留 `original`。
- Chat 的供应商 `video_url` 扩展在原始 Chat 转发中保持原样。进入 Responses 或 Messages 转换时明确返回 400；不生成没有协议依据的 `input_video`，不把视频丢弃后只发送旁边的文本。音频跨协议转换不在此次实现范围，桥接遇到音频也明确拒绝，原始音频端点转发不变。
- 工具媒体从结果中提取后仍携带原调用 ID 的归属提示。图片和 PDF 会作为后续 user 内容交给模型，而不是让 base64 数据在转换中消失。

生成参数方面，Messages `stop_sequences` 直转 Chat 时保留为 `stop`，JSON Schema 输出约束在三协议间按各自格式映射，Chat/Responses 间保留 `verbosity`、`logprobs` 与 `top_logprobs`（含零值）。Responses 没有 `stop` / `top_k` 对应字段，Chat 的 penalty、seed、logit_bias、多个候选及 stop 也不能由现有 Responses 桥接表达；这些显式参数会得到 400，而非被解码后静默忽略。Messages 没有 logprobs 或低/高 verbosity 对应控制，同样明确拒绝。原生同协议请求保留这些参数，已有按实际模型限制 sampling 的规则不变。

永久回归覆盖实际转发入口的 400、未发出上游请求、文档进入实际模型请求体、原生 Chat 视频/供应商参数直通、媒体内工具关联，以及 JSON Schema / 参数零值。全部使用本地模拟上游，不验证真实视频模型可用性，也不调用付费 API。

## 实现来源

起点为用户提供的 `SUB2API_SSE_PATCH_AB_MIGRATION_20261106.md`，附录 A/B 摘要分别为：

```text
A f1799eb45e3acbf4da30fb01be53ce72e96c8d3ecd3aa842f4fe3c95a57eb138
B 824c547bb8b0a3b6617f227bf087f6bc355e2bf7a5ab8907ff52ac2ebd71a1fa
```

在 production 基线 `3077b050730c322969e04568f384ae5008e7cebb` 上适配，补充原生 WS 逻辑 attempt、完整事件边界、最终出口投影和新功能提示。交付方提供的发布提交及生产测试结论不是本 PR 的独立运行证据。未复制插件二进制、重新签名或引入第二套连接池。
