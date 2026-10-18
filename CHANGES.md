# CHANGES — VKMDownload 变更记录

> 每次改完代码都在这里记一笔：日期、改了什么、为什么改。
> 发版 / push 前看这份文件写 release notes。

### 50. 审计 A2 批：引擎杂项 + 命令行（UNRELEASED）
- P2-3 拒 HEAD 的服务器不再傻等：HEAD 回 405/501/403 以前被当作"暂时性"，会再重试 3 次、每次间隔翻倍，共约 3.5 秒才去发第一个真正能用的 GET。现在 HEAD 被拒即放弃，直接走 GET 探测。验收：`TestProbeNoHeadMakesExactlyOneHead`（HEAD 恰好 1 次、探针 < 0.5 秒）、`TestHeadUnavailableIsNotRetryable`（整次下载也只发 1 个 HEAD）。
- P2-4 服务器返回的数据范围对不上时立刻失败：以前按"暂时性"错误重试，默认退避下最长卡 151 秒才报错。现在归为不可重试，实测 0.01 秒失败、每个连接的请求只发 1 次。验收：`TestBadContentRangeFailsFast`（默认退避下 < 1 秒、请求数 ≤ 连接数）、`TestErrRangeUnusableIsNotRetryable`（起点/终点/总长/缺头四种错都带哨兵且不可重试；"服务器多给了尾部"这种真实行为仍然放行）。
- 探针自身的 206 校验：以前只要 206 返回了任何 Content-Range 就算"支持分段"，于是首发 206 撒谎的服务器会被规划成多连接、每个块都失败，错误却指向块而不是探针。现在要求 `bytes=0-0` 得到的就是 `0-0` 且总长可见。验收：`TestProbeRejectsLying206`（错误终点、错误起点、总长未知、无法解析四种都判为不支持分段并给出原因）。
- Content-Type 以真正的 GET 为准：HEAD 说是纯文本、GET 说是网页时，以前会因为 HEAD 压过 GET 而把网页当文件下载；反过来 HEAD 说是网页、GET 说是文件时又会误拒。验收：`TestProbeGetContentTypeWins`。
- 弱 ETag 不再用于 If-Range：`W/"..."` 按 RFC 9110 不能出现在 If-Range，服务器会拒或忽略，续传就会把两个版本拼在一起。现在自动退到 Last-Modified，没有就留空。验收：`TestValidatorSkipsWeakETag`。
- 没有校验值时拒绝续传：服务器既不给 ETag 也不给 Last-Modified 时，旧代码硬着头皮从上次的偏移接着下。只要文件在服务器上被替换成同名的另一份，拼出来的就是长度正确、内容错位的坏文件。现在这种情况丢弃半成品、从头下。验收：`TestResumeWithoutValidatorRestarts`。
- `VerifySize` 从死开关接上：选项和 `-verify-size` 以前没有任何读取点，开与不开行为完全一样。现在开启后除"长度等于服务器公布的大小"外，还要求"长度等于本次真正写入的字节数"，两者不一致就拒绝发布。验收：`TestVerifySizeChecksReportedBytes`（计数 90 / 文件 100 时拒绝；一致时正常发布）。
- 进度记录写入失败不再静默：失败只累加一个没人读的计数、外加一行需要开调试才看得见的日志。现在第一次失败就通过新的 `Request.Logger` 报告（之后每 100 次报一次，避免刷屏），并计入 `Result.StateSaveErrors`。验收：`TestSaveFailureReachesLogger`、`TestSidecarWatermarkAdvancesOnlyOnSaveSuccess`。
- 文案说人话：分段失败以前一律写 "ended with bytes unclaimed"，把"重试耗尽"和"计划本身算错了"两种完全不同的情况混成一句看不懂的话。现在分别写"重试耗尽，最后一次错误是……"和"这段计划的字节数对不上"。验收：`TestSegmentFailureMessageSaysWhy`。
- 重试计数改为每次下载独立：以前是全进程一个计数器，每开一个新任务就清零，正在跑的任务的数字会被别人改掉、结果也不准。现在每次 `Download` 自带一份。验收：`TestRetriesCountedPerCall`（并发两次下载，第一次仍报出自己的 2 次重试）。
- `Limiter.Wait` 参数改 `int64`：字节数在 32 位构建上是 32 位 int，超过 2 GiB 的块会溢出成小数或负数，限速直接被跳过。验收：既有 limiter 测试全部改用 int64 并通过 `-race`。
- ETA 钳位：速度在极短窗口测出来时，剩余秒数换算成 Duration 可能溢出成负数或荒谬数值，界面就会显示怪倒计时。现在 NaN/Inf/超过上限一律钳住。验收：`TestETANeverNonsense`。
- 退避参数防 NaN：`Factor` 或 `Jitter` 为 NaN 时所有比较都是 false，会绕过全部上下限，算出 NaN 延迟等于不退避。现在非有限值按无效处理，超大重试次数饱和到上限。验收：`TestBackoffRejectsNonFinite`、`TestBackoffDelayIsCapped`（还有新增的 `backoff_test.go` 六个用例，此前该文件零测试）。
- 隐藏属性失败记日志；顺带修掉一个实测确认的真 bug：`GetFileAttributesW` 失败返回 `0xFFFFFFFF`，旧代码用 `^uintptr(0)` 比较，在 64 位上是 `0xFFFF...FFFF`，永远不相等，于是一个不存在的文件会被当成"已隐藏"，还会把 `0xFFFFFFFF` 当属性写回去。验收：`hide_windows_test.go`（只读属性在隐藏/取消隐藏后保持不变；不存在的文件安静跳过且 `isHidden` 返回 false）。
- `Close()` 注释与行为对齐：注释写"保留调试句柄以免泄漏"，代码实际是关掉句柄、下次按需重开——后者才对，改的是注释。
- 文件名两项：`filename*=` 在裸 `filename=` 形式下不解析（`mime.ParseMediaType` 拒收时）——现在支持 UTF-8 与 Latin-1，扩展形式优先；180 字符按字节截断会把中文/emoji 切成半个字符——现在按字符截断，并额外限制 UTF-8 字节数不超过 240 以免撞 NTFS 255 上限。验收：`TestFilenameStarInBareDisposition`、`TestFilenameRuneSafeTruncation`。
- 命令行：默认 chunk/reserve 改为 0 交给引擎 `Normalize`（以前硬编码 128K/512K，把引擎已否定的旧值打了回去，高延迟源上慢好几倍）；重复 `-H` 改 `Header.Add`（以前用 `"; "` 拼成一个值，两个 Cookie 拼成一条畸形 Cookie，其它头第二个值直接消失）；新增 `-limit`（KiB/s，所有连接共享同一个 Limiter）；文件大小未知时显示 "unknown (server did not say)" 而不是 "-1 B"；进度条以前拿新行长度和旧宽度比，永远不会补空格，旧行尾巴擦不掉；bench 的 `MinSizeForSegments` 改 1。验收：`cmd/idm/main_test.go` 15 个用例（参数重排、布尔 flag 不吃 URL、`--` 后以 `-` 开头的 URL、重复 `-H`、`-limit`、引擎默认值、`humanBytes` 边界、`roundDuration`、进度打印 `-race`），该包此前零测试。
- 门槛：根 `go vet` / `go test ./... -count=1`（engine 44.2s、cmd/idm 1.3s）、`internal/engine` `-race`（65.4s）、`GOOS=linux go vet ./...`、desktop `go vet` / `go test`（31.8s）、desktop `GOOS=linux go vet`、前端 `tsc --noEmit` / `vitest run`（30/30）、扩展 `node --test`（27/27）全部通过。

### 51. 审计 B 批：下载列表生命周期（UNRELEASED）
- P1-4 快速"暂停→继续"把任务写成"已取消"：`startRun` 以前先起 goroutine、由 `run` 内部才把任务代数 +1，于是"已发出启动"到"真的跑起来"之间那段窗口里，任务身上还挂着上一轮的代数。用户在这段窗口里连续点暂停+继续，旧那一轮的退出分支看到任务"正在下载"（其实是新一轮刚写的状态），就把旧的意图盖上去——新一轮显示成"已取消"，接着 Cancel 的清理还会删掉刚起步那一轮的半成品。现在代数在 `startRun` 持锁的提交点自增并传给 `run`，退出分支必须 `t.generation == gen` 才能改状态。验收：`TestPauseResumeRaceKeepsStatusHonest`（60 轮背靠背暂停/继续，状态一次都没被旧轮改坏）、以及 `-race` 全套。
- 顺带修掉同一条链上的竞态：任务在"正在启动"或"最后几字节落地"时被用户删除，旧代码照样把进度、失败或完成写回那条已经不在列表里的记录，屏幕上的行会被"复活"。现在 `taskStillListed` 统一判定，进度回调、失败分支、整理分支、完成分支四处都过这道闸。验收：并发删除的既有用例 + `-race`。
- P1-5 设置页与后台的 DATA RACE：`normalizeOrganizeGroups` 用 `groups[:0]` 复用调用方的数组原地改写，而 `Config()` 返回的结构体虽然按值拷贝，`OrganizeGroups` 这个切片却仍指向同一块底层数组。于是设置页改一项就等于绕过锁改真实配置——审计用 `-race` 实锤。现在规范化总是新建切片、`Exts` 也拷贝，`Config()` 与 `SetConfig()` 都走 `clone()` 深拷贝。另外 `SetConfig` 落盘失败会回滚内存里的值，不再出现"界面显示已改、磁盘没改"的不一致。验收：`TestConfigAliasCannotMutateLiveSettings`、`TestConfigConcurrentSetAndReadIsRaceFree`（并发 200 轮读写，`-race` 无报警）。
- P2-8 "重试中"提示被写进 tasks.json：恢复等待期间那条提示以前会在 Pause 时被一起存盘，重启后这条早就不会重试的任务照样闪"N 秒后自动重试"。现在 Pause、Cancel、`loadTasks`（重启恢复）三处都清空提示，停止分支补上了落盘。反过来，提示本身现在会在进入等待时就落盘——等待可能长达一分钟，中途崩溃应该回来看到"当时在等重试"，而不是一条假装还在下载的记录。验收：`TestRecoveryNoticeNeverPersistedForStoppedTask`（进入等待→暂停→读 tasks.json，无 notice；再模拟重启，首帧也无旧 notice）。
- P2-9 失败提示把带令牌的完整链接写进文件并显示：`humanError` 缺 429/408/500/502/503/504 分支，落到兜底直接返回原始错误，而引擎错误里包着请求 URL——带签名的下载链接 query 就是凭据，于是 `token=...` 同时进了 tasks.json 和屏幕。现在先整段脱敏（`redactURIs` 把每个 http(s) URL 换成"主机+路径"），再按状态码映射，兜底也绝不回原文。验收：`TestHumanErrorNeverLeaksTokens`（六种含 `token=SUPERSECRET123` 的错误形态，输出里都搜不到）。
- P2-10 全局计划被无关任务干扰：全局计划以前用**整张表**的 paused/failed/canceled 计数判断"用户是否介入"，于是计划跑到一半新加一个无关的暂停任务，计划的完成提示音和关机就被悄悄跳过；而"选中任务计划"只看自己的任务，同一个动作在两个按钮下结果不一样。现在两个 watcher 共用 `planCoverage`（计划任务集合）+ `scanPlan` 一条规则：集合内任务暂停/失败/取消=介入，完成=干净结束，运行/排队=继续等；计划期间新启动的任务会被纳入集合，避免关机切到正在下的文件。验收：`TestPlanIntervenedSemantics`、`TestStartPlanRunsToCompletion`、`TestStartPlanWithFullSlotsStillRuns`、`TestStartPlanSelectedPropagatesFreeze`。
- P2-11 迟到的进度把失败行"复活"：done/failed 事件直发，进度走队列，于是失败后排队里的旧进度会晚一步到达，把行重新画成"下载中"。前端只有弱守卫，现在后端泵里就丢掉已终结任务（完成/失败/取消/已删除）的进度帧。验收：`TestLateProgressCannotReviveFailedRow`（失败任务的排队帧被丢弃；同一帧对运行中任务仍会投递，证明测的是泵而不是"什么都不发"）。
- 配置损坏不再静默丢弃：`loadConfig` 以前解析失败直接返回默认值，用户的设置凭空消失且毫无痕迹。现在把坏文件改名留证（`config.json.corrupt-<时间戳>`，与 tasks.json 的做法一致）并写日志。验收：既有配置用例 + 启动路径。
- `saveConfig` 补 `Sync()`：与 tasks.json 一致，崩溃不会留下"文件名正确、内容没落盘"的设置文件。
- 其他 manager P3：panic 分支读任务 ID 改为过锁（原来唯一一处裸读 view）；`req.Conns` 在 API 层钳到 [1,64]（与配置上限同一常量 `maxConns`）；`wails.Run` 失败同时写进应用日志（以前只有 `println`，用户找不到）；tasks.json 临时名带 pid，两个实例共用数据目录不再互相截断；引擎的"进度记录写入失败"经 `Request.Logger` 进应用日志。
- 门槛：根 `go vet` / `go test ./... -count=1`（engine 44.9s、cmd/idm 1.3s）、`internal/engine` `-race`（66.8s）、`GOOS=linux go vet ./...`、desktop `go vet` / `go test`（34.3s）、desktop `-race`（37.5s）、desktop `GOOS=linux go vet`、前端 `tsc --noEmit` / `vitest run`（30/30）、扩展 `node --test`（27/27）全部通过。
- 审计探针固化：`%TEMP%\opencode` 下三个手工 overlay 探针（config 竞态、Notice 落盘、Pause/Resume 竞态）已改写成 `desktop/audit_b_test.go` 里的正式用例，不再依赖临时目录里的文件。

### 52. 审计 C 批：自动更新器（UNRELEASED）
- **P0-2 真机验证结果（与审计结论不同，先说结论）**：审计给的判断是"备份用硬链接 + 助手从 `.bak` 启动 = 覆盖自己正在运行的映像，`MoveFileEx` 可能 60 秒全败"（75% 确信）。我在本机（Windows 11 26100 / NTFS / 同卷）用真实 PE 做了对照实验：备份为硬链接、从 `.bak` 启动、替换原路径 → **成功，约 2ms**；备份为副本走同流程 → **成功，约 3ms**；让进程从**被替换的那个路径本身**启动再替换 → **失败，Access is denied**。也就是说审计描述的那条链在本机没有复现，真正会失败的是"替换自己这张映像"这一条（助手从 `.bak` 启动恰好避开了它，因为替换只是删掉一个目录项，旧数据仍由另一个链接活着）。**仍然改成一律复制**：复制让"备份 / 正在运行的映像 / 被替换的目标"变成三个互不相同的文件，回滚与清理不必再依赖链接语义，代价是每次更新一次 ~13MB 拷贝。实验脚本放在 `%TEMP%\updprobe\`，不在仓库里。
- P1-6 换文件失败后桌面上没有程序：替换失败时旧 exe 其实完好无损，但启动它的那个进程已经退出，于是"更新失败"的结果是**一个程序都没在跑**，用户只能自己去双击。现在失败分支会把旧版拉起来；助手侧对 staged 校验失败时同样会拉回。另外 `rollback()` 遇到 exe 已经被改名/不存在时会直接拒绝（"回滚失败"），使 UI 的「还原旧版本」在这类状态下永远无效；现在目标缺失时直接把备份放回原位。验收：`TestSwapFailureLeavesTheOldVersionInPlace`（断言恰好启动一次且是旧 exe）、`TestRollbackRestoresWhenTheExecutableIsGone`、`TestRollbackStillRefusesWithoutABackup`。
- P1-7 连点两次更新会写坏同一个文件：并发护栏在网络请求**之前**，检查与置位分开，两次点击都能通过检查，然后各自 `O_TRUNC` 打开同一个 staged 文件交错写入，之后必然校验失败——而且卡在 downloading 态连重试都不行。现在"检查 + 占用"在同一把锁内完成，占用之后才发请求，网络失败回退 failed。验收：`TestStartUpdateClaimsTheSlotBeforeTheNetwork`（第二次调用不触发第二次查询）、`TestStartUpdateRejectsConcurrentCalls`。
- P1-8 正在应用更新时还能开始新下载 / 助手不核对文件：`applying` 现在被 `StartUpdate` 直接拒绝（否则新下载会写进助手正要安装的那个文件）；助手侧 `validateStaged` 以前只查 "开头是 MZ 且大于 1MiB"，一个被截断的下载完全满足——现在按应用记录的大小与 SHA-256 复核，状态文件新增 `newSize` 字段。验收：`TestStartUpdateRefusesWhileApplying`、`TestValidationRefusesATruncatedStagedFile`。
- P2-2 自更新下载句柄不关："新版文件校验失败已丢弃"是假话——失败分支在句柄还开着的时候 `os.Remove`，Windows 上必然 sharing violation，错误被 `_ =` 吞掉，文件留在原地，下次尝试再撞上。成功路径也从不在关闭前 `Sync`。现在统一 `discardStaged()`（先关再删、删除失败记日志），成功路径 `Sync` 后再关闭。验收：`TestStagedFileHandleIsReleasedOnFailure`（失败后文件不存在，且能重新创建——能创建即证明句柄已释放）。
- P2-5 慢启动被误判失败：60 秒超时只看"到点那一刻"有没有窗口，于是"窗口出现过但随后最小化/自己隐藏"被当成从未启动，白回滚一个能用的新版。现在等待期间**只要出现过一次窗口**就记为 unconfirmed（保留新版并提示），从未出现才算 timeout。验收：`TestHandshakeSeesAWindowThatClosed`、`TestHandshakeTimesOutWithNoWindowEver`。
- P2-6 握手盖章不校验是不是自己：任何被塞了握手参数的 exe 都能替别人的更新"确认"，随后下一次启动按这条记录删掉备份——而备份可能是唯一能用的副本。现在盖章前要求：运行映像路径等于记录里的 exe、记录带 `NewSHA`、且映像哈希与之相符；`cleanupStaleUpdate` 里 `NewSHA==""` 时不再删备份，且只有**真的删掉备份**才删标记。验收：`TestHandshakeWillNotConfirmFromAnotherBuild`（三种拒签形态）、`TestCleanupKeepsTheBackupWithoutAHash`、`TestCleanupKeepsTheBackupWhenTheDeleteFails`。
- P2-7 两个进程抢写同一个状态文件：应用与助手各写 `update-state.json`，无锁读改写会让助手的 `launched` 被应用滞后的 `backed-up` 覆盖，下次启动就说"更新没发生"；原子写的临时名还是固定 `.tmp`，两边互相截断。现在加跨进程锁文件（超时后有界放弃并记录，过期锁文件自动清理），临时名带 pid + 序号，`markStage` 的读-改-写在锁内完成。验收：`TestStateLockSerialisesWriters`（8 并发写、不留 .lock/.tmp）、`TestMarkStageKeepsConfirmed`。
- 更新 P3：无 `Content-Length` 且无 digest 时拒绝（否则任何"前 1MB 完好"的截断文件都能过关）；更新放弃后把原本在下载的任务恢复（以前一律留在暂停，像是应用偷偷停了用户的活）；路径过长时预检报错并给出建议（`MAX_PATH` 追加 `.bak`/`.failed-<时间戳>` 会超）；`WRITE_THROUGH` 注释改成不夸大（它是请求，不保证数据落盘，真正保证靠写入端 fsync）；助手校验 `--exe/--bak` 必须是自身映像（否则这个程序就是一个"拿参数替换任意文件"的工具，放在 `main.go` 里做，因为那里才知道真实映像）；"已是最新"不再写成 failed，改为独立状态 `current`，前端设置页与横幅各自处理；`.new` 残留超过一天清理。验收：`TestDownloadRefusesWithoutLengthOrDigest`、`TestFreezeRestoresWhatWasDownloading`、`TestPreflightRefusesAPathTooLongForTheBackupName`、`TestAlreadyCurrentIsNotAFailure`。
- 门槛：根 `go vet` / `go test ./... -count=1`（engine 45.1s、cmd/idm 1.2s）、`GOOS=linux go vet ./...`、desktop `go vet` / `go test`（35.1s）、desktop `-race`（38.2s）、desktop `GOOS=linux go vet`、前端 `tsc --noEmit` / `vitest run`（30/30）、扩展 `node --test`（27/27）全部通过。本批新增 19 个用例，都在 `desktop/audit_c_test.go`。

### 53. 审计 E 批：浏览器扩展 + 无感配对认证（UNRELEASED）
- **P0-3 本地接口零认证（本轮最严重）**：以前扩展只凭 `/ping` 返回的 `app:"VKMDownload"` 就认定对方是应用，然后把自己的 Cookie（含 HttpOnly，通过 cookies API 读到）和带签名的下载链接 POST 过去；端口一旦被缓存就不再复查。也就是说：**本机任何程序**抢到 18888 就能拿走用户的登录身份。现在改为无感配对：
  1. 应用首次运行生成 per-install 随机密钥（32 字节，存 `%AppData%\VKMDownload\pair.key`，权限 0600，**从不写日志**）。
  2. 新增 `GET /pair`：必须同时满足"Origin 是 chrome-extension:// 或 moz-extension://"**且**"连接进程是可执行签名校验通过的浏览器"才返回密钥，否则 403。这个接口**无视** `VerifyPeerProcess` 开关——它发的是凭据，开关关掉也不等于谁问都给。
  3. `/ping?challenge=<hex>` 回 `X-VKMDownload-Proof: HMAC-SHA256(key, challenge)`；扩展用 `crypto.subtle` 校验。挑战必须是 hex 且 ≤128 字符（空挑战**不**回 HMAC，否则空串的 HMAC 可被重放）。
  4. 扩展把密钥存 `chrome.storage.local`；端口**只有验证通过后才缓存**，worker 重启后重新验证，不再永久信任任何端口。
  5. 拿不到密钥（未配对）先调 `/pair`；被拒 → 按"应用未运行"处理，**绝不**把 URL/Cookie 发给未验证的响应方。
  验收：`desktop/audit_e_test.go` 9 例（密钥只生成一次且跨进程复用、两个数据目录不共享密钥、损坏密钥文件被替换、证明随密钥与挑战变化、空/非 hex/超长挑战不回应、`/pair` 无浏览器 Origin 403、`/pair` 非签名浏览器 403、`/pair` 只接受 GET、密钥不出现在日志里）＋ `extension/test/pairing.test.js` 5 例（只会回 ping 正文的假服务器被拒且**一次 /add 都没收到**、配对一次后复用、`/pair` 被拒时什么都不发、未验证时不读 Cookie、不接管别的扩展的下载）。
- P1-11 应用拒绝时**取消用户本来正常的下载**再裸重下：这条路会丢掉进度、Referer 和用户选的目录，最坏（重下也失败）什么都不剩。现在拒绝路径只报告，**不动浏览器里那条下载**。测试期望同步改正（原用例断言的正是被删掉的旧行为）。
- P1-12 popup 开关没有错误处理：service worker 不可达时 `sendMessage` 会 reject，旧代码让按钮**永久变灰**且无提示。现在 `try/catch/finally`：失败时刷新状态、恢复按钮、把错误简短显示在提示行（截断到 80 字符）。
- P1-13 取消与抹记录之间没有状态确认：cancel 可能失败（下载刚好完成），旧代码照样 `erase`，于是**下完的文件历史被抹掉**、应用又下一份。现在先看 `lastError`，再重新 `search` 确认不是 `complete` 才 `erase`；并且取消前先把 id 写进 `chrome.storage.session`。
- P2-12 端口扫描最长串行 11×600ms（还要加策略与尺寸等待）：现在 11 个端口**并行**探测，总耗时约 600ms 封顶；已知 `totalBytes` 时不再轮询尺寸。
- P2-13 交接成功到取消之间 worker 被杀 → 双份下载：`chrome.storage.session` 记"已交接待取消"的 id，worker 启动时 `drainPendingCancels()` 补齐取消。
- P2-14 fallback 自我拦截循环：拒绝路径不再产生 fallback 下载（随 P1-11 一起消失），`ownDownloads` 仍保留作为第二道保险。
- P2-15 `/add` 的 Origin 依赖：未做真机抓包（本轮无浏览器自动化），因此**没有**放宽应用侧校验；配对之后即使某版本浏览器不带 Origin，扩展也会先因拿不到密钥而按"应用未运行"处理、绝不会把凭据发给别人。这一点明确标注为**未验证**。
- P2-17 扩展没有标准测试入口：新增 `extension/package.json`（`"test": "node --test \"test/*.test.js\""`，`private: true`，要求 node ≥18）；`npm test` 32/32 通过。
- 扩展 P3：不接管别的扩展发起的下载（`item.byExtensionId`）；通知与提示文案统一清洗（URL 只留主机+路径、去控制字符、超长截断），避免把带签名 query 的链接显示到通知里；popup 长文案换行（`overflow-wrap: anywhere`）。
- 门槛：根 `go vet` / `go test ./... -count=1`（engine 44.9s、cmd/idm 1.4s）、`GOOS=linux go vet ./...`、desktop `go vet` / `go test`（34.6s）、desktop `-race`（38.1s）、desktop `GOOS=linux go vet`、前端 `tsc --noEmit` / `vitest run`（30/30）、扩展 `node --test`（32/32）全部通过。
- 顺带修掉的测试脆弱点：`TestStartEmitsProgressImmediately` 原来用"立刻连接失败"的地址，两个任务可能在 StartAll 走到第三个之前就失败并释放槽位，使"启动了 3 个"变成合法结果；现改用慢速真实服务器，断言才有意义。

### 54. 第 4 阶段：专项安全/隐私审计与修复（UNRELEASED）
审计方式：4 个只读 subagent 分头做（S1 落盘与外发、S2 本地 API 攻击面、S3 扩展隐私、隐私文档对账），
执行者只读复核后自己动手修。审计基线 `e1da072`，修复见 `desktop/privacy.go`、`desktop/pair.go`、
`desktop/peercheck*.go`、`desktop/api.go`、`extension/background.js`、`PRIVACY.md`。
新测试：`desktop/audit_hardening_test.go`、`desktop/audit_stage4_test.go`、`extension/test/pairing.test.js`。

**先说最要紧的三条（都是审计实测/推理确认的真问题）：**

1. **peer check 可用 `127.0.0.2` 绕过（高危）**：`tcpOwnerPid` 只匹配"本地地址 == 127.0.0.1"，而
   Windows 的 loopback 是整个 `/8`。任何本机程序把源地址绑到 `127.0.0.2` 再连 18888，查表查不到
   → 旧代码 fail-open → 直接放行，`/pair` 的密钥、`/add`、`/settings` 全部可用。现在：查表同时
   匹配对端（本地端口 = 客户端端口、远端 = 本监听端口、两端都在 127/8），且**任何查不到/进程读不到
   一律 fail-closed**。验收：`TestPeerCheckFailsClosedOnLookupError`、`TestPeerCheckAdmitsSignedBrowser`。
2. **peer check 只认文件名 + 任意有效签名（高危）**：把系统签名的 `curl.exe` 复制成
   `%TEMP%\chrome.exe` 即可通过（复制不改内容、签名仍有效）。现在额外要求映像位于浏览器安装根
   （Program Files / Program Files (x86) / LOCALAPPDATA），拒绝时日志写明路径。验收：
   `TestPeerCheckRejectsUnknownExe` 等 + 新增根目录检查。
3. **`/add` 与 `/settings` POST 不校验调用者是不是"我们的扩展"（中高）**：Origin 只能说明"某个
   扩展"，peer check 只能说明"某个浏览器"。于是任何已安装扩展都能往队列塞下载（可带任意 Cookie
   访问内网 = SSRF）。现在这两个写操作要求 `X-VKMDownload-Auth: <unix>:HMAC(key, method\npath\nunix)`
   （±2 分钟窗口、常量时间比较、绑定方法与路径），扩展侧同步实现签名。验收：
   `TestSignedRequestIsBoundToMethodAndPath`、`TestStartUpdateRefusesWhileApplying` 同层、
   扩展 `pairing.test.js`。

**落盘数据（S1）：**

4. `tasks.json` 曾把**每一次**下载的完整 URL 连同签名参数一起存下来（本机实测：329 条里 69 条带
   query，含 `token`、`sig`、`jwt`、`X-Amz-Signature`、Azure SAS 的 `se/skt/skoid`）。现在
   **已完成**的任务落盘时 URL 去掉查询串、Cookie/Referer/UA 直接清空；未完成（暂停/失败/取消）
   仍保留，因为续传要原样重放。验收：`TestFinishedTaskLosesItsSignedURL`、
   `TestUnfinishedTaskKeepsWhatItNeeds`。
5. 日志曾是凭据仓库：本机实测 900 行里 173 行带签名 URL（历史版本写的）。现在 `debugLog` 在写入
   前统一脱敏（所有 http(s) URL 只留 host+path，正则大小写不敏感），User-Agent 只记长度，更新握手
   的文件名（含 token）不再写日志；启动时一次性清洗已有的 `tasks.json` 与日志。验收：
   `TestDebugLogSanitisesEveryLine`、`TestLogsDoNotCarryUserAgentOrHandshakeToken`。
6. 崩溃/损坏留下的副本也是同一个泄露面：`tasks.json.corrupt-*`、`tasks.json.*.tmp`、
   `tasks.backup-*`。现在启动时一并清洗（带 cookie 的直接删除，其余去 URL 查询串），
   `ClearDownloadHistory` 也会删掉它们。验收：`TestScrubRemovesCorruptAndTemporaryCopies`、
   `TestClearHistoryRemovesCopiesToo`。
7. "清除数据"以前有竞态：清空内存表后直接删文件，并发的 `saveTasks` 可能把旧记录写回来。现在整段
   持 `saveMu`；并且**绝不删用户下载好的文件**。验收：`TestClearDownloadHistoryKeepsTheFiles`。
8. 记录保留期：已完成/已取消/已失败超过 90 天的记录在启动时清理，未完成的不动。验收：
   `TestTrimHistoryDropsOnlyOldFinishedTasks`。
9. 文件被移走的任务以前会被标成 failed，用户点"继续"却因为 URL 已被脱敏而永远失败。现在这种任务
   的 Resume 直接给出"原文件已不在保存位置，请重新添加下载链接"。验收：`TestFileGoneTaskRefusesResume`。

**本机接口（S2）：**

10. **无 Host 头校验（DNS rebinding 纵深缺失）**：现在只接受 `127.0.0.1` / `localhost` / `[::1]`
    （可带端口），其他 Host 一律 403。验收：`TestHostHeaderMustNameThisMachine`、
    `TestLoopbackHostAcceptsEverySpelling`。
11. CORS 从 `*` 改为回显调用方 Origin + `Vary: Origin`（`*` 本身不可利用，但没有理由留着一个
    "任何来源可读"的头）。
12. `/ping` 只接受 GET/HEAD；`/add` 入口统一校验 scheme（以前 `file://`、`javascript:` 会被转发到
    新建任务弹窗，等用户确认后才在 Start 里失败）。验收：`TestAddRejectsNonHTTPSchemes`。
13. 配对证明的挑战规则收紧：空挑战不回 HMAC（否则空串的 HMAC 可被重放）、非 hex、超长一律拒绝。
    验收：`TestPingAnswersOnlyAWellFormedChallenge`。

**扩展（S3）：**

14. **每次交接前重新证明应用身份**：以前 `apiBase` 缓存后不再验证，应用退出后任何程序占同一端口
    就能直接收到 URL + Cookie。现在 `ensureApp()` 在每次 `/add` 前重新做一次 HMAC 挑战（多一次
    本机往返，几毫秒）。配对失败也不再静默重配：已存密钥但证明不过时按"应用未运行"处理，弹窗提供
    「重新配对」按钮——静默重配等于把密钥交给先占端口的程序。
15. **先证明、再读 Cookie**：拦截与右键两条路径以前是先 `cookiesFor()` 组 payload、再找应用，现在
    顺序反过来；本机/内网地址**不读 Cookie**（manifest 给了 `127.0.0.1` 权限，否则本地开发站的
    会话 cookie 也会被读走）。
16. **隐身模式不接管**（`item.incognito` / `tab.incognito` 直接返回）：否则隐身下载会出现在任务
    列表、`tasks.json`、日志和通知里。
17. 来源页只发"协议 + 主机 + 路径"，去掉查询串（搜索词、文档 ID 不再进任务记录）。
18. 通知与提示统一脱敏（`sanitizeText` 以前是死代码）：URL 只留 host+path、去控制字符、超长截断；
    `/add` 请求加 `redirect: "error"`（带 Cookie 的 body 不该被重定向到别处）。
19. 不接管其他扩展发起的下载（`item.byExtensionId`）。

**隐私文档（对账结论：不是"承诺了没做到"，而是"只写了扩展"）：**

20. 仓库里唯一一份隐私声明是 `extension/PRIVACY.md`，只覆盖扩展；桌面端把 Cookie/URL 写盘、
    启动访问 GitHub、删除能力有限，文档一句没提。现在新增产品级 `PRIVACY.md`（根目录），逐项写明：
    每个文件存什么、为什么、何时清；外发只有"下载本身 + 启动查更新"（不含安装 ID/机器标识/任务
    数据）；配对密钥是什么；怎么清除；以及 5 条**明确标注的取舍**（未完成任务 Cookie 明文、sidecar
    含签名链接、日志不脱敏文件名与路径、更新检查无开关、同步盘会带走这些文件）。扩展那份补上配对
    密钥、`pendingCancel`、HttpOnly/父域 Cookie、沉浸式例外，并在顶部指向产品级文档；两份 README
    都加了链接。
21. 更新检查的 User-Agent 从 Go 默认的 `Go-http-client/1.1` 改成 `VKMDownload/<版本>`，防火墙日志
    里能看出是谁在访问。README 里"需要登录的网盘链接无法下载"已过时（扩展会带 Cookie），一并更正。

**门槛**：根 `go vet` / `go test ./... -count=1`（engine 46.4s、cmd/idm 1.4s）、`internal/engine`
`-race`（65.5s）、`GOOS=linux go vet ./...`、desktop `go vet` / `go test`（35.1s）、desktop `-race`
（39.2s）、desktop `GOOS=linux go vet`、前端 `tsc --noEmit` / `vitest`（30/30）、扩展 `node --test`
（32/32）全部通过。

**明确未验证（不猜测）**：浏览器在 no-cors GET 时是否发送 Origin；`cookies.getAll` 在无站点权限时
返回空还是抛错；`storage.session` 在崩溃恢复场景是否严格清空；Chrome PNA 对删除 `127.0.0.1`
host 权限的影响；`prepare`/`Compose` 类命令在真机的实际行为；更新请求的实际网络字节（未抓包，
代码层面确认只发 `Accept` 与 `User-Agent`）。

**残余风险（已记录，未修）**：配对没有 native messaging 那种真正的信任锚点——如果恶意程序在用户
**首次**配对的那一刻就占住端口，它仍可能被当成应用。修法需要加 native messaging host（注册表 +
host manifest）或 TLS 证书固定，属于架构改动，需要你拍板；当前缓解是"配对一次后不再静默重配"、
"每次交接重新证明"、"`/pair` 强制 peer check"。另外未完成任务仍明文存 Cookie（续传需要），
如需加密需要引入 DPAPI 与密钥管理。

## 2026-10-18 — 0.2.7

### 48. 稳定性收尾批：测试基建、重启语义、并发防护、更新说明与大小交接（UNRELEASED）
- 测试基建：Windows 下 `TempDir` 清理的假报警根治。`Manager` 新增 `bgWait`/`goBg`，把传输 goroutine、删除/取消的 settle waiter、文件名探测、计划 watcher 全部纳入跟踪；`newTestApp` 在 `t.TempDir()` 之后注册 `t.Cleanup(WaitBackground)`，按 LIFO 在清目录前先等后台静默。为什么改：此前清理瞬间仍有 goroutine 持着 `tasks.json` 或半成品在写，"directory not empty" 间歇性地把无关测试拖红，真 bug 反而淹在噪声里。验收：desktop 全量连跑 3 次 0 失败；engine 82 过；扩展 27/27；`tsc` 0 错；vitest 22/22。
- 重启语义：`loadTasks` 只把 `downloading` 折回 `paused`，`queued` 保持排队；`startup` 在 `Run` 之后补一次 `pumpQueue`，重启后继续等并发位，有空位就开跑。为什么改：排队任务本来就是"用户要下、只是在等槽位"，以前一并改写成暂停，等于重启后静默取消了用户的排队意图，得手动全部开始。回归：`TestLoadTasksKeepsQueuedTasksQueued`。
- 状态机加固——同一任务并发双启动的原子防护：`startRun` 在持有任务锁时发现状态已是 `downloading` 就放弃本次启动（`cancel()` 后返回），冻结与槽位已满两条分支也不再改写正在传输的任务。为什么改：双击恢复、两个 settle 点同时泵队列、`StartAll` 撞上刚释放的槽位，都会起第二个引擎写同一个半成品文件；第二次 `startRun` 还会覆盖 `cancel`/`done`，第一次 run 从此停不下来。回归：`TestStartRunRefusesASecondLaunch`。
- 状态机加固——`Cancel` 拒绝已完成任务：直接返回"已完成的任务无法取消，直接删除即可"。为什么改：以前会把成品记录改成"已取消"，随后 `Remove(deleteFiles=true)` 判定 `wasComplete=false`，静默拒绝删除用户明确要删的文件，记录与实际行为对不上。回归：`TestCancelRefusesCompleted`。
- 状态机加固——取消删半成品前重读文件名。为什么改：取消时文件名可能还没被探测或首个进度事件填上，旧快照拼出的路径是"`<目录>.part`"，永远匹配不到真实半成品，所谓"丢弃"其实留下了垃圾。
- 状态机加固——`StartPlan` 在更新冻结时明确拒绝（返回 `errUpdating` 并复位 `planRunning`），计划 watcher 与选中计划 watcher 也纳入 `goBg` 跟踪。为什么改：冻结不是一份排队的工作，硬启动只会得到一个所有任务都拒绝启动、最后注定记成"介入"的 watcher，按钮提示也会撒谎。
- 更新链路——新增"更新内容"弹窗：横幅与设置页两处「立即更新」都先看说明再确认下载。内容由后端 `ReleaseNotes(version)` 从 GitHub release 的 tag API 实拉，与发布页同源；版本号只允许数字与点（进 URL 前的白名单），响应体 1MiB 上限、请求 10 秒超时；同版本结果缓存在 `App` 里，重复打开不重复拉；离线或该版本没写说明时显示降级提示，不拦更新。为什么改：更新是用户点头才发生的动作，得先知道换来了什么；说明只能现拉——编译进包里的文字永远描述不了这个版本之后才发布的版本。
- 更新链路——`tasks:loaded` 从 `startup` 挪到 `domReady`。为什么改：Wails 不缓冲事件，启动阶段 emit 时 JS 监听器还不存在，这条"恢复完成后刷一次"的通知从前端视角等于从未发出，首屏拿到空表时没人纠正。
- 更新链路——自我更新下载加 10 分钟总超时。为什么改：默认 client 没有读超时，服务器中途断流会让状态永久停在"下载中"，而 `StartUpdate` 在该状态下拒绝重入——用户除了重启程序没有别的重试办法。
- 更新链路——拉 `latest` release 的响应体同样限 1MiB；`OpenReleasePage` 改为解析 URL 后逐项校验（`https`、host `github.com`、路径在本项目 `releases/` 下），并拒绝含 `..` 的路径段。为什么改：字符串前缀校验挡不住 `releases/../../owner/repo` 这类拼接，所谓白名单形同虚设。
- 扩展——回环 API 的拒绝理由（Origin 不合法、peer check 不通过）改用 JSON 返回。为什么改：扩展按 JSON 解析错误体，程序发纯文本时它只能退回兜底文案，通知里永远只显示"HTTP 403"，真正的原因（比如被拒的可执行文件名）全丢了。
- 扩展——交接请求带上浏览器已知的 `totalBytes`（`onCreated` 先取一次，`downloads.search` 再刷新一次）。为什么改：扩展拦下的任务以前进列表显示 0B，必须手动开始才知道大小；浏览器本来就知道，顺手带过来即可，也不用额外发请求。
- 扩展——测试 22 → 27：新增 `report` 的四种结果（pending / duplicate / 成功 / 拒绝）、`toggle` 的 403、策略缓存 3 秒 TTL、自启下载记录的上限与结算清理。为什么改：通知文案、开关错误路径、缓存过期和记录上限都是最容易悄悄回归的边界。
- 端到端大小传递：`/add` 接收 `size`，`location:ask`（询问位置）事件也带 `size`，`StartRequest.Size` 落到任务行，前端从到达队列经 `dialogSize` 传到 `NewTaskDialog` 的 `initialSize`。为什么改：同一条信息在四个环节里丢一次就白传；现在暂停中的任务一出现就有真实大小，同内容查重也多了个可用信号。测试：`TestLocalAPIAddCarriesSizeBeforeStarting`、`TestLocalAPIAddAsksLocationWithSize`。
- 发版脚本：预检改成"两处版本字段最终等于 `$Version`"，允许版本号提前提交（发布 commit 与 tag 的源码包保持一致）；`git fetch` 后拦截 `main` 上未 push 的 commit；扩展安装包打包时剔除 `test/`；构建沿用 `-trimpath`，产物不带构建机路径。为什么改：旧的"替换零命中即失败"会把提前提交版本的正常流程误判为失败；而未 push 的 commit 会让 tag 指向远端旧树，且出错时毫无提示。
- 前端小修——手动「新建下载」补上 `dialogOpenRef`。为什么改：到达队列靠这个 ref 判断弹窗是否已被占用，漏设时剪贴板/扩展任务一到达就会覆盖用户正在输入的表单。
- 前端小修——`ConfirmDialog` 与更新说明弹窗的 Esc 在捕获阶段 `stopImmediatePropagation`。为什么改：全局 Esc 还会关掉背后的设置页/新建弹窗，设置页上开着更新弹窗时一次按键关两层。
- 前端小修——列宽防抖在 `TaskList` 卸载时兜底落盘。为什么改：切筛选或打开设置会卸载列表，挂起的 300ms 定时器随组件消失，整段拖拽的宽度改动全部白费。
- 有意不做（附原因）：程序端不主动探测未知大小——探测请求会消耗一次性下载链接的 token，为了不显示 0B 而把链接烧掉不值，0B 只是难看；`formatWhen` 跨午夜的 60 秒缓存窗口——触发条件苛刻且 60 秒后自愈，记录在案不修。

### 49. 自更新链路重写：换文件搬进程序自身，唯一事实来源是握手文件（UNRELEASED）
- 事故复盘（0.2.5 →「立即更新 → 重启生效」）：弹出两个黑终端窗口，等了很久，程序没变成新版，事后目录里是主 exe（新版）+ `.bak`（旧版）+ `.new`（又一份新版），没有任何进程在跑。两个根因：(a) `ApplyUpdate` 调 `runtime.Quit` 却没先 `requestQuit()`，而 Wails 的 `Frontend.Quit` 会先问 `OnBeforeClose`，"最小化到托盘"（默认开）把它变成隐藏窗口并否决退出——程序压根没退，旧脚本白等 30 秒，任务永久冻结在 updating；(b) 旧 PowerShell 脚本每轮重试都先 `Remove-Item $Bak`，第一轮改名成功、移动失败后，第二轮就把唯一的旧版删了，循环耗尽后主 exe 与备份都不存在，脚本因 `Test-Path $Exe` 为假而不启动任何程序（临时目录里无签名的 exe 被 Defender 锁住是移动反复失败的原因）。
- 换文件搬进程序自身（新增 `update_swap.go` / `update_swap_windows.go` / `update_swap_other.go`）：`ApplyUpdate` 先把当前 exe 备份成 `exe.bak`（优先 `CreateHardLink`，同卷零拷贝；不支持时回退 `CopyFile` 并校验大小一致），再用 `exe.bak --apply-update …` 启动助手。助手必须从 `.bak` 启动——Windows 允许重命名运行中的 exe，但不允许覆盖它。
- 原子替换：`MoveFileEx(staged, exe, REPLACE_EXISTING|WRITE_THROUGH)` 单步完成，主 exe 路径没有空窗，失败时旧版原样留在原地。失败按 0.2s/0.5s/1s/2s/3s 退避重试，总上限 60 秒（对付 Defender 锁新写入的 exe），每次记 `GetLastError`（用 `%w` 包装，可按 errno 判定 sharing violation，不依赖系统语言）。
- 握手确认：旧进程用 `crypto/rand` 生成 32 字节 token（只允许 64 位十六进制进文件名），新版带 `--update-handshake <token>` 启动，在 `domReady` 原子写 `%AppData%\VKMDownload\update\<token>.ok`（不写 startup：Wails 不缓冲事件，startup 发的 emit 前端收不到）。助手每 200ms 查：文件出现且 pid 匹配=成功；自己启动的 pid 已退出=立即失败；超时 60 秒=失败。超时后唯一的次证据是"pid 还活着**且**有可见主窗口"——只活着不算（活着但没握手按失败回滚），有窗口才算"成功但未确认"：那时保留新 exe 与 bak 并提示，误回滚比不确认更糟。
- 回滚：失败时 kill 自己记录的那个 pid（不误杀用户手动开的实例）→ 等它退出 → 把坏 exe 改名成 `exe.failed-<时间戳>` 留证 → `MoveFileEx(bak, exe)` 还原 → 启动旧版 → 标记 `rolled-back`。任何一步都不删 bak；只有新版确认活着才由新版在下次启动异步清理（助手运行时删不掉自己的映像），并且 `confirmed` 标记不会被后来失败的助手降级覆盖。
- 阶段标记 `%AppData%\VKMDownload\update-state.json`（与 tasks.json 同目录；不能放 exe 目录，安装版在 Program Files 不可写）：`prepared → backed-up → replaced → launched → confirmed / rolled-back / aborted`，原子写（tmp+rename），同时记录新旧 exe 的 SHA-256，供下次启动判断当前跑的到底是哪个版本。
- 下次启动只提示、绝不自动还原：新增 binding `ConsumeUpdateNotice() string`（前端主动拉，返回后写 `notified=true`，保证只提示一次；带 `notified` 字段而不是复用 `a.upd`，免得被 `checkUpdateOnce` 覆盖）+ `RollBackUpdate()` 手动还原。`startup` 里只做纯文件读，并在确认过的情况下清掉 bak 与标记。理由：程序能启动就说明 exe 在，更新其实成功只是没确认时自动还原等于把新版换回旧版。
- `ApplyUpdate` 加固：`ready → applying` 由后端自己挡重入（前端 disabled 只是辅助）；更新前预检 staged（>1MB、`MZ` 头、SHA-256 与下载时记录一致）、exe 目录可写、磁盘余量，且都放在冻结任务之前（拒绝一次只花一次点击，冻结后拒绝要赔上一次暂停）；`runtime.Quit` 前必须 `requestQuit()`；Quit 后 10 秒看门狗，仍活着就 unfreeze 并报失败。删除 `writeUpdaterScript`。
- 隐藏终端：统一 `hiddenCmd`（`CREATE_NO_WINDOW` + `HideWindow`；只靠 HideWindow 仍会创建并闪一下），用于启动更新助手、`shutdown`、`app.go` 两处 `cmd /c start`（`explorer` 是 GUI 子系统，不用管）；完成提示音改成直接调 `kernel32!Beep`，不再为一声提示拉 PowerShell。
- 下载校验补强：`downloadUpdate` 把算出的 SHA-256 存进 App 字段供 `ApplyUpdate` 用；无论 GitHub 是否给 digest，都校验 `written == ContentLength`（已知时）、文件 >1MB、前两字节是 `MZ`；digest 为空时写日志。
- 测试：换文件逻辑拆成可注入的小函数（`validateStaged` / `backupExe` / `swapExe` / `retryUpdate` / `waitForExit` / `awaitHandshake` / `markStage` / 标记读写），系统调用（复制、硬链接、替换、启进程、等进程、杀进程、时钟、日志）经 `updateSys` 接口注入；全部在 `t.TempDir()` 里用普通文件冒充 exe，注入时钟让 60 秒退避瞬间跑完，绝不碰真实 exe。覆盖：替换失败（旧版内容不变、bak 在、未启动新进程、bak 未删）/ 重试三次后成功且退避序列正确 / 60 秒放弃 / 备份回退到复制 / 截断备份被拒 / staged 各种坏文件 / 握手成功 / 别人的握手不算数 / 新版秒退回滚+失败证据 / 握手超时回滚 / 活着且有窗口不回滚 / 旧进程不退出→不碰文件标 aborted / 启新版失败回滚且旧版不丢 / `ApplyUpdate` 退出被否决必须 unfreeze 并返回错误（本次事故回归）/ 标记损坏不删任何东西 / 通知只消费一次 / 各阶段文案 / confirmed 不被降级 / `hiddenCmd` 带 `CREATE_NO_WINDOW` / Windows 真实锁：`CreateFile` 不带 `FILE_SHARE_DELETE` 打开目标→replace 返回 sharing violation 且旧内容不变，关句柄后同一调用成功。
- 下一步（未完成）：真机端到端（装 0.2.5 → 更新到 0.2.6）量一下 Defender 锁与 WebView2 冷启动的实际耗时，再决定握手 60 秒超时是否要调大；版本号待 bump。前端已接：`App.tsx` 挂载时拉一次 `ConsumeUpdateNotice`，有话要说的更新事件（回滚/未确认/未开始）以横幅常驻显示直到手动关掉；设置 → 软件更新 里在后端明确"可在设置中手动还原"时显示「还原旧版本」按钮（走 `RollBackUpdate`，同一隐藏助手执行还原）。

## 2026-10-15 — 0.2.5（已发布，https://github.com/Vick-moo/VKMDownload/releases/tag/v0.2.5）

### 1. 托盘退出后进程残留（bug）
- 症状：关闭窗口最小化到托盘后，点托盘 退出，图标没了但进程还在后台跑。
- 根因：Wails Windows 的 `Frontend.Quit` 会先问 `OnBeforeClose`（`desktop/main.go:47` 绑的 `app.beforeClose`），而 `beforeClose` 在 `MinimizeToTray` 开时永远返回 `true` 拦截，于是托盘的 退出 和 `Quit` binding 都被否决。
- 改动：
  - `desktop/app.go`：`App` 加 `quitFlag atomic.Bool` + `requestQuit()`；`beforeClose` 见到置位直接放行；`Quit()` 先置位再 `runtime.Quit`；`appVersion` 升到 `0.2.5`。
  - `desktop/tray.go`：退出 handler 先 `requestQuit()` 再 `runtime.Quit`（图标清理走 `shutdown/stopTray`）。
  - `desktop/manager_test.go`：加 `TestBeforeCloseQuitIntentLetsTrayExitThrough`。
  - `desktop/wails.json`：`productVersion` 升到 `0.2.5`。
- 验证：`go vet` 通过；`TestBeforeClose*` 通过；`go test ./...`（desktop）全绿。

### 2. 多连接在部分服务器上全 0 B（bug）
- 症状：`dl.fuckingfast.co` 的任务多连接下载全是 0 B，失败信息是 `segmented download failed ... bytes unclaimed`；单连接能下（实测 25 秒 386 MB）。
- 根因：该站无视 Range 尾端——请求 `bytes=0-524287` 却回 `Content-Range: bytes 0-2097151999/2097152000`（body 倒是正确的前缀）。引擎 `validateRangeResponse` 要求尾端精确相等，于是每块都失败；错误是 retryable 所以重试 10 次，最后 `fetchSegment` 吞掉真错只报通用的 `bytes unclaimed`，界面永远 0 B。
- 改动：
  - `internal/engine/download.go`：`validateRangeResponse` 接受 `end >= off+n-1`（只读前 n 字节）；`fetchSegment` 循环尾保留 `lastErr` 并写进报错信息。
  - `internal/engine/download_test.go`：加 `TestDownloadRangeEndIgnoredByServer`（httptest 模拟无视尾端的服务器，多连接逐字节校验）。
- 验证：新测试通过；`go test ./...`（engine，约 31s）全绿。
- 备注：`tasks.json` 里 10 个 `queued` + 0 B 是预期行为（`AutoStart` 关 + `MaxConcurrent: 2`，任务只入列没启动，probe 没跑所以 size 未知），不是 bug。


### 3. restart turns never-started queued into paused (pending)
- New tasks with AutoStart off are queued by design (manager.go Start).
- But loadTasks (manager.go:1167) rewrites queued to paused on restart.
- Harmless: Resume still starts them. Only slightly imprecise semantics.
- User informed; leaving as-is until user decides.
### 4. Plan download refused when concurrency slots full (bug)
- Symptom: with tasks queued and MaxConcurrent slots occupied (e.g. by stuck
  0B downloads), Plan button and Start All appear dead: plan reports no
  waiting tasks.
- Root cause: StartPlan only counted tasks newly started by that call
  (manager.go StartPlan). Slots full means started==0, so it errored and the
  watcher never launched. Turning everything paused would NOT fix it: gated
  tasks stay queued either way.
- Fix: active + queued counts as a valid plan; watcher launches and pumpQueue
  pulls queued tasks in as slots free. Returns covered count for the UI.
- Test: TestStartPlanWithFullSlotsStillRuns. Existing plan tests unaffected.

### 5. Deleting a record leaves junk in Downloads (fixed)
- Symptom: deleting tasks from the list left .part files behind (13 orphans
  with URL-token names in Downloads). Two sources: engine never cleaned up
  after failed downloads, and record deletion never touched disk.
- Decision with user: deleting an UNFINISHED task auto-removes its .part +
  sidecar (they can never resume without the record). Finished files are
  never auto-deleted. Same rule flows into bulk delete and ClearFailed.
  Re-adding the same URL restarts from zero.
- Changed: desktop/manager.go Remove, README delete section, App.tsx confirm
  texts (x3), TaskRow trash title, new test TestRemoveDropsUnfinishedPartials.
- Verified: new test passes, go vet clean, tsc clean.

### 6. Extension hands over empty filename (bug, fixed)
- Symptom: files grabbed from the browser land in Downloads named after URL
  tokens with no extension (e.g. 2GB fuckingfast files unrecognizable).
- Root cause: extension/background.js onCreated handler handed over
  item.filename from creation time, when Chrome usually does not know it yet.
  The 250ms wait for size was not reused for the name.
- Fix: re-search the download item after the size wait and hand over the
  fresh filename when present. Only helps servers sending Content-Disposition
  and only future downloads; already-landed token files need manual rename.
- Verified: node --check clean. Needs extension reload in browser to take
  effect (unpacked extension: chrome://extensions reload button).

### 7. Probe ignores Content-Disposition from ranged GET (bug, fixed)
- Symptom: fuckingfast files land with URL-token names even though the server
  sends Content-Disposition with the real name (RFC 5987 filename* form).
- Root cause: probeHead fills SuggestedName from the URL token when HEAD has
  no disposition; probeRange then refused to recompute it (guarded by == "")
  even after learning the real disposition. The probe knew the name but never
  used it. Manager already adopts probe names on first progress, so no UI
  change needed.
- Fix: probeRange promotes a disposition-derived name over the URL fallback.
- Verified: new test TestProbePrefersDispositionOverURLToken passes; live
  `vdl info` on a token URL now reports the real .rar name.
- Side finding: these dl links are single-use/expiring (a used one 404s).
  Start remaining parts promptly.

### 8. New tasks paused by default + duplicate URLs rejected
- New tasks added without Start now rest PAUSED (was queued): clicking one
  task starts only that one, the rest never auto-follow. Start All / plan
  still pick up paused tasks, so bulk flows are unchanged.
- Same URL re-added to the same folder is rejected with the existing record
  (no second transfer fighting over one .part, no burnt single-use tokens).
  Same URL to another folder is allowed; completed re-downloads allowed.
- Extension: duplicate handover answers 200 (no browser fallback double
  download) with an already-listed notification.
- Dialog: on duplicate it closes, selects the existing row and flashes.
- Tests: TestNewTasksRestPaused, TestDuplicateURLIsRejected. Full desktop
  suite green with no old-test changes needed.
- Verified: go vet clean, tsc clean, wails build green (bindings regenerated).

### 9. Add-time probe: instant names+sizes, content-based duplicates
- New tasks with no browser filename are probed once at add time (10s cap,
  fail-open, 1 retry): the row shows the real name and size immediately
  instead of blank/0B, and the probe powers duplicate layer two.
- Layer two: different URLs, same host + filename + compatible size are the
  same file (single-use token links) and re-adds are rejected. Identical
  URLs stay on layer one (another folder is allowed); completed exempt.
- Dead links cost ~4s on add (then created unnamed as before); live links
  ~0.5s. Transfer re-probes anyway, so add-probe retries stay minimal.
- Tests: TestSameFileDifferentURLIsRejected. Full desktop suite green.

### 10. Add-time probe fast-fail for dead hosts
- The 4s per dead-link add was our own retry sleeps, not the server.
- Fix: 2s TCP dial pre-check (hostReachable); unreconnectable hosts skip the
  probe entirely. Live links unaffected. Proxy users keep old behaviour
  (dial bypasses proxy, fail-open).
- Result: probe-related tests 20s -> 4.5s; full desktop suite 89s -> 21s.
- Test: TestHostReachableFastFails. Exe rebuilt.

### 11. Ask-location dialog hidden behind browser (fixed as far as possible)
- Symptom: with ask-location on, the placement dialog opened behind the
  browser; user had to minimise the browser to find it.
- Fix: new BringToFront binding (Unminimise + Show + always-on-top flash)
  called when ask/clipboard dialogs open. Wails v2 is single-window, so a
  detached small window is NOT possible; whole window comes forward with the
  modal dialog. Alternative offered: turn off ask-location for zero popups.
- Verified: vet + tsc clean, wails build green. Foreground behaviour needs
  human verification (Windows may still only flash the taskbar).

### 12. Multi-select bulk actions (done, field editing still pending)
- Toolbar shows when rows are selected (click, Ctrl+click to add):
  continue / pause / delete-selected with per-task settle counting, plus
  clear-selection. Delete follows the unfinished-partials rule.
- Uses existing per-task bindings; no backend change. Original TODO's
  batch field editing (connections/destination) still open.

### 13. Selection UX redo: checkbox + scope-switching buttons
- Per user: always-visible checkbox at the right of each name (no edit mode);
  row click / Ctrl+click still work.
- Top buttons switch scope when rows are checked: start/stop/delete act on
  the selection with counts in labels; plan stays all-tasks. Removed the
  extra selection bar. Delete-visible also clears stale selection.
- Verified: build + tsc clean.

### 14. Scoped plan download + checkbox left (per user)
- Plan button joins the scope switch: with checked rows it runs
  StartPlanSelected (only those ids started and watched; removals and
  cancels count as intervened and skip end actions). No selection means
  the global plan as before. finishPlan refactored into finishPlanEnd.
- Checkbox moved to the LEFT of the name (convention; user asked to be
  stopped on illogical ideas, rule added to both AGENTS.md).
- Fixed two stale comments (ClearFailedTasks/RemoveTasks records-only).
- Test: TestStartPlanSelectedCoversOnlyTheSelection. Full suite green.

### 15. Delete missed dir-destined partials (bug, fixed)
- Symptom: every delete flavour left .part/.json behind for tasks created
  without a filename. Root cause: Remove/Cancel deleted dest+suffix, but
  those tasks keep Dest as the DIRECTORY while the engine resolves
  dir+server-name underneath, so the deletion pointed at a nonexistent path.
- Fix: partialPaths() mirrors the engine resolution (dir+view.Name when
  Dest is a directory); used by Remove and Cancel. User's 4 orphan files
  deleted manually (records long gone, unreachable by code).
- Test: TestRemoveFindsPartialsByServerName. Full suite green, exe rebuilt
  and app restarted.

### 16. Start button looked dead on slow links (fixed)
- Symptom: clicking start-all changed nothing on screen for seconds.
- Root cause: startRun changed status but emitted no event; UI waited for
  the engine's first progress (seconds away on slow links).
- Fix: startRun emits task:progress synchronously on every path
  (downloading, queued-behind-limit, update-parked). Covers single,
  bulk, plan and pumpQueue starts.
- Test: TestStartEmitsProgressImmediately (asserts 2 downloading + 1 queued
  events with zero waiting). Also caught and fixed a test-side deadlock
  (never hold a lock across StopAll: its paths emit too). Full suite green,
  exe rebuilt + restarted.

### 17. UI refresh rate setting (fast/medium/slow)
- New setting in Settings: fast 250ms (today's behaviour, default),
  medium 650ms, slow 1000ms. Controls engine progress-report interval only;
  transfer speed untouched; applies to newly started runs.
- Wiring: Config.RefreshRate + normalize + refreshInterval() in run();
  select dropdown in SettingsPage. Old configs without the key fall back
  to fast (no visible change on upgrade).
- Test: TestRefreshIntervalMapping. vet/tsc/build/tests green, app restarted.

### 18. Queued rows can now pause + speed-limit self-check documented
- Queued rows showed only resume; backend Pause always accepted queued.
  Frontend now shows pause on queued rows too (stops auto-queue), plus
  space-shortcut support. No backend change.
- User speed report diagnosed as self-imposed: speedLimitKb=1000 caps the
  total at ~1MB/s (matches the 1.1MiB/s screenshot exactly). Fix is settings
  change to 0, not code. part01 0B row has a dead (expired) link: re-click
  from the site for a fresh token.
- Verified: build + tsc clean, app restarted.

### 19. Settings apply live: concurrency + speed (no restart needed)
- Raising MaxConcurrent pulls queued tasks in at once (SetConfig pumps only
  on increase). Lowering never kills running transfers; the queue gate
  honours the new value by itself.
- Speed limit retunes the shared limiter in place (new Limiter.SetRate):
  running transfers feel it on their next block. Unlimited included.
- Fixed a latent race found on the way: progress snapshots taken outside
  the lock could be delivered out of order (TestProgressIsMonotonic failed
  on a clean tree too). Emission now clamps to a delivery watermark.
- Tests: TestLimiterSetRateAppliesLive, TestApplySpeedLimitRetunesLive.
  Engine + desktop suites green incl. -race on progress/limiter. Exe rebuilt
  and app restarted.

### 20. Refresh rate applies live too
- SetConfig now pushes the cadence to every running transfer
  (livedl registry in Manager + Downloader.SetProgressInterval; trackers
  read the atomic value per block). No restart, no re-probe.
- Tests: TestDownloaderProgressIntervalLive,
  TestApplyRefreshIntervalReachesLive. Engine + desktop green incl. race.
  Exe rebuilt and app restarted.

### 21. Column drag ripples to other dividers (bug, fixed)
- Symptom: dragging one header divider moved other columns too.
- Root cause: the borrow loop spilled to further neighbours once the
  adjacent one hit its minimum, changing the total and triggering the
  proportional rebalancer across all columns.
- Fix: a divider moves only its two sharing columns by equal and opposite
  amounts (growth bounded by what the neighbour can surrender); total stays
  constant so layoutWidths is a no-op and every other divider is untouched.
- Verified: build + tsc clean, app restarted. Needs human drag check.

### 22. Column drag blinks past the stop (fixed)
- Symptom: holding the drag past a column's limit flickered the list.
- Root cause: every mousemove issued setPrefs (new array identity) even
  with zero change, repainting the list and rewriting localStorage at
  pointer rate.
- Fix: early return when the clamped width equals the current one.
- Verified: build + tsc clean, app restarted. Needs human drag check.

### 23. Size column too narrow
- Widened size/added-time column default 104->124, minimum 84->104.
  Note: widths persist in localStorage, so existing installs keep old
  widths until a divider is double-clicked to reset. Exe rebuilt+restarted.

### 24. Column drag snaps back at the limit (bug, fixed)
- Symptom: pulling past a column's stop made the edge jump back and blink.
- Root cause: the growth cap mixed the drag-start base with the LIVE
  neighbour room, so the cap collapsed to the start value the instant the
  neighbour ran out - one frame snaps the column back, the next fights
  forward again.
- Fix: clamp the absolute pointer target against current-value bounds only.
  At the stop the target equals the current width and the early return
  holds it still.
- Verified: build + tsc clean, app restarted. Needs human drag check.

### 25. Column drag cascades through pinned neighbours
- Request: dragging the divider left of a column that needs space should
  automatically push pinned columns further along, no manual pre-moving.
- Behaviour: dragging a boundary moves it; the growing side takes, the
  shrinking side gives nearest-first down to minimums; the far side is
  never touched and the total stays constant (rebalancer stays out).
  Exhausted side means the edge stops; same-array return skips renders.
- Verified: build (incl. tsc via vite) + tsc clean, app restarted. Human
  drag check needed.

### 26. Column drag runaway (bug, fixed)
- Symptom: a small drag flew the divider to the screen end.
- Root cause: the clamped delta (absolute from drag start) was ADDED onto
  live state every move instead of recomputed from the drag-start base -
  quadratic accumulation.
- Fix: snapshot prefs at mousedown; every move rebuilds from that base.
  1:1 tracking restored.
- Verified: build + tsc clean, app restarted. Human drag check needed.

### 27. Helper files hidden in Explorer
- .part + sidecar are now created with the Windows hidden flag, so the
  download folder shows only finished files (same idea as system files).
- Rename preserves the flag, so publish explicitly unhides the finished
  file - otherwise completed downloads would stay invisible. No new
  dependency (stdlib kernel32 via build-tagged files; no-ops off Windows).
- Test: TestHelperFilesHideAndPublishUnhides. Engine suite green,
  linux/darwin builds verified. Exe rebuilt + app restarted.

### 28. Finished rows flipped back to downloading (bug, fixed)
- Symptom: tasks complete on disk and in tasks.json, UI stuck at 100%
  downloading until clicked. Backend exonerated by evidence twice.
- Root causes, both event ordering: (1) overlapping full-list reloads with
  no generation guard let a stale fetch paint over task:done; (2) progress
  travels a queue while done jumps it directly, so late stale progress
  resurrected finished rows.
- Fix: reload applies only the latest call; progress handler ignores
  non-completed updates for completed rows (failed excluded: resume
  legitimately returns them to downloading).
- Verified: build + tsc clean, app restarted.

### 29. Optimization batch (5 items)
- Async add-time probe: Start returns at once, probe fills name/size via
  event; untouched probed duplicates remove themselves (fillProbedName).
- Bulk index writes coalesced: batch() makes StartAll/StopAll/RemoveTasks/
  RemoveFailed/StartPlanSelected write tasks.json once (kept Indent: humans
  read this file when diagnosing).
- Progress pump keeps newest on saturation instead of dropping it.
- TaskRow memoized (+ stable onChanged); update check single-sourced to the
  mount query (event subscription removed).
- Also fixed: progress out-of-order delivery watermark (pre-existing race,
  failed on clean tree too), racy test assertion, TempDir flakes noted.
- Verified: desktop + engine suites green, race on progress/limiter clean,
  tsc clean, exe rebuilt + restarted.

### 30. Resume-flush knobs: per-MB, per-time, per-percent
- Settings has three independent sidecar knobs (KB / ms / % of total, 0
  disables the percent one): whichever fires first writes. Report and save
  throttles are fully split in the tracker now.
- Debugging footnote: two cancel/resume tests failed mid-way; root cause was
  testOptions normalizing BEFORE setting ProgressInterval, pinning save
  thresholds to the 250ms default while reporting ran at 50ms. Production
  run() always sets every field explicitly - unaffected. Fixed the fixture.
- Tests: extended TestRefreshIntervalMapping, TestSaveThrottlesIndependently
  (percent case). Suites green, tsc clean, exe rebuilt + restarted.

### 31. Settings tabs + flush switches + reorder
- Settings page is tabbed (Chrome-tabs style): download / extension / plan /
  clipboard / done / window / update. Sections stay mounted so drafts survive.
- Download tab grouped: parallel / speed / display / resume-flush; ask-location
  and organize sink to its bottom. Extension up, clipboard down, update last.
- Resume-flush triggers are individually switchable (any subset, at least one
  forced on); numbers disable with their switch.
- Footnote: block moves via script broke JSX twice (truncated take + brace
  drift); repaired against git pristine, verified by counts + tsc.
- Verified: tsc clean, build green, app restarted.

### 32. Flush checkbox restyle + swap
- Number box left, checkbox right; checkbox redrawn for the dark theme
  (accent fill + white tick) instead of the native box.
- Verified: tsc + build clean, app restarted.

### 33. Flush rows redesigned (subtitle + switch, box below, hint last)
- Per user layout: sub-title row with the on/off switch at right, number box
  on the next row, description under it. Uses the existing Switch component.
- Verified: tsc + build clean, app restarted.

### 34. Flush cards per mockup
- Trigger cards: group card with accent-bar title, per-trigger card with
  title + number + switch right, hint below. Removed the checkbox restyle
  dead CSS. Verified: tsc + build clean, app restarted.

### 35. Flush rows back to native design language
- The card mockup was layout reference only; restyled into plain .setting
  rows (title + hint left, number + switch right), matching every other row.
  Removed the flush card CSS. Verified: tsc + build clean, app restarted.

### 36. UI small fixes (UNRELEASED, UI only)
- Row switches vertically centered against number boxes.
- Clipboard interval hint notes monitoring must be on first.

### 37. Concurrent-duplicate guard + extension size wait (UNRELEASED)
- Manager: url+dest inflight claim in Start, so same-instant /add arrivals
  yield one task and duplicates (was: N tasks for N racers, seen 3x same URL).
  Regression test TestStartConcurrentSameURL (fails 16-vs-1 without fix).
- Extension: sizeKb polls up to ~1.5s for totalBytes before answering unknown,
  so the min-size filter sees real sizes instead of bypassing on -1.

### 38. Extension 1.0.2 store update (pushed, store review pending)
- Manifest 1.0.1 -> 1.0.2 (update submissions need a higher version).
- Store zip rebuilt with the sizeKb fix; package replaced on Partner Center,
  review clock restarted.
- #37 commits pushed to main; no GitHub release yet.

### 39. Engine correctness batch (UNRELEASED)
- fetchOnce commits actual written bytes (was: whole block), restoring the
  split/outstanding invariant. Regression: TestCommitPartialKeepsClaimOutstanding.
- Size<0 barred from segmented path (was: fake-success empty file).
- Segmented completion always verifies known length; VerifySize now only
  controls extra checks.
- Disk write errors (PathError/SyscallError/ShortWrite) no longer retried.
- Sidecar watermark advances only on successful save (+saveFails counter).
- Limiter bucket scales with rate (small limits actually limit now).
- debugFile closed in Downloader.Close; ChunkSize honored on segmented path;
  NoRange branch made explicit error. 9 new tests in repair_test.go.

### 40. Backend persistence batch (UNRELEASED)
- tasks.json persists manual/referer/cookie/userAgent (restart no longer
  kills private tasks or mis-organizes manual ones).
- paused/failed count as plan-intervened (no more surprise shutdown/sound).
- organize falls back to copy+remove cross-volume, uniqueDest (1) suffix,
  failures surface in task.Error.
- Optional pairing auth: ApiToken (auto-generated) + ApiAuthRequired
  (default off); Bearer guard on /add + settings POST.
- /add field caps (URL/Referer 8KB, filename 255 rune); content-layer
  duplicate re-check in insert critical section + pickDuplicateVictim.
- saveMu + fsync; corrupt tasks.json preserved as tasks.json.corrupt-*;
  completed only flipped on IsNotExist; seq reseeded from loaded IDs;
  clipboard URL trim shared helper; debugLog mutex; StartPlanSelected
  counts real resumes; log URLs redacted to host+path. 15 new tests.

### 41. Frontend UX batch (UNRELEASED)
- files:dropped wired (drag a link opens the dialog); update:available
  subscribed (offline-start banner no longer lost) + release-page button.
- Dialog arrivals queue instead of overwriting; selected + keyboard delete
  unified behind ConfirmDialog; selected pruned against live tasks.
- canceled filter/sidebar entry; clipboard switches made mutually exclusive;
  minSegmentSize setting added; min-size now raw KB input; empty organize
  rows blocked with a hint; per-tick scans merged to one pass.
- Column first-frame guard, width cap proportional, sidebar width persisted;
  ConfirmDialog Esc/mask close; empty-filter back button; row delete confirm;
  row errors flash. vitest harness: 22 tests (format/columns/filterLabel).

### 42. Extension no-loss batch (UNRELEASED, needs browser reload)
- Recognizes pending (no longer fake-success); POST-before-cancel ordering;
  completed browser items left alone; fallback uses fresh filename.
- ownDownloads mirrored in chrome.storage.session (best-effort: manifest
  keeps 4 permissions for the pending store review); fallback try/catch.
- Pairing token field in popup (Bearer header, 403 shows pairing hint);
  fresh policy on status; selection-text CJK punctuation stripped.
- node:test suite: 22 tests (decision + manifest contract locks).

### 43. Release + docs batch (UNRELEASED)
- release.ps1 preflights: existing tag / dirty tree / version-replace hits.
- CODEMAP section 4 rewritten to actual throttle/flush behavior, dup entry
  merged, section 6 covers extension + release flow, handover order fixed.
- CHANGES TODO deduped. update_test.go: offline-silence + allowlist tests.

### 44. Peer process verification (UNRELEASED, default on)
- Loopback now confirms the caller is a genuine signed browser (TCP owner
  PID + exe allowlist + Authenticode, cached per exe) before honoring /add
  and /settings. Forged Origin curl is rejected; failures logged with exe.
- Settings switch (top of extension section) with ms cost in the hint
  (~1ms per download, ~300ms once per new browser exe). Reads live config:
  toggle applies instantly, no restart. Lookup infra failures fail open.
- 8 new tests (allowlist, admit/reject/signature/fail-open, end-to-end
  200/403, cache, real-sig manual-only after a Defender false positive).

### 45. Pairing token removed everywhere (UNRELEASED)
- Peer check made the token redundant (same-user malware reads both copies),
  so it is gone: backend fields/bindings/guard, settings pairing section,
  popup token box, background Bearer + pairError paths, PRIVACY row.
- Popup keeps version line + refused-error display; manifest keeps storage
  (ownDownloads session mirror still uses it). Net -439 lines.

### 46. Recheck fixes round (UNRELEASED, self-fixed after read-only audit)
- Peer: WTD_STATEACTION_CLOSE=2 (was 5, leaked handles), empty-table panic
  guard, revocation-off documented, single-Stat cache with normalized capped
  keys, exe path buffer + error wraps, dup comments merged.
- Extension: failure fallback cancels the original first (was: double
  browser download); toggle reads fresh policy (was: stale-cache double
  flip). New cancel-before-download regression test (node 22 total).
- Frontend: arrival race via dialogOpenRef; NewTaskDialog resets on all
  initials; runSelected in-flight guard; dead clearFailedTasks wrapper out.
- Docs: README 4-setting table + peer-check security section; PRIVACY
  cookie scope + notifications/session rows; Draft comment fixed.

### 47. 性能优化批（UNRELEASED，只读性能审计后自查自改）
- 引擎：ReserveSize 默认 512KiB→16MiB、上限钳 64MiB（高延迟源不再被"每 512KiB 一个 RTT"卡死单 worker ~10MB/s；claim 是记账不是内存，读仍按 ChunkSize 走）；ChunkSize 默认 128KiB→256KiB（与池块对齐，每字节的 WriteAt/commit/限速税减半）；分段下载启动时 Truncate 预分配到完整大小（高段写入不再按需零填空洞、碎片减少，尽力而为失败不阻断）；sidecar 改紧凑 JSON；splitLargest 改切"claim 之后的未认领区"（旧规则要求无在途 claim，而 worker 全程持 claim，动态分片实际从未触发过；新切点保证 claim 全留在头段）；进度回调改 TryLock 丢弃 + finish() 走阻塞 emitFinal（慢 UI 不再串住 worker，终帧 100% 必达）。测试更新：claim 内不越界切分、全被 claim 时拒绝切分。
- 后端：pumpQueue 单快照（原来每启动一个排队任务都全表 List+activeCount，StartAll 上百任务 O(N²)→O(N logN)）；loadTasks 的 completed Stat 挪出 m.mu（断网盘卡 Stat 不再冻结 List/Stats/Start）；List 改单次管理器锁；新增 countByStatus 一遍扫描（Stats/计划观察器 500ms 轮询不再全表拷贝+排序，statsIntervened 并入 taskCounts.intervened）；批量删除合并为单 waiter goroutine + 一次 pumpQueue（原来每行一个 3s waiter 各自唤醒扫全表）；同内容查重按任务缓存 host（add 高频不再每候选 url.Parse）；文件名探测加 4 并发信号量（扩展 burst 添加不再几十个并发 dial）；进度泵饱和改整队倾泻（不再回放旧位置造成进度回跳）；run() 选项构造里贴了两遍的 ResumeFlush 块删除。
- 启动：tray/文件拖放/更新检查提前到 loadTasks 之前；config.json 只在文件缺失时写（原每次启动无条件重写+触发杀软实时扫描）；恢复完成后发 tasks:loaded 事件、前端收到即 reload（修"首屏 listTasks 撞上恢复中拿到空表且无人纠正"的竞态）；release.ps1 构建 -trimpath（去 exe 内本机路径，可复现构建）。
- 体积：third_party/systray 内置 fyne.io/systray v1.12.2 单补丁副本——上游 systray_notifier_unix.go 缺 //go:build 约束（_unix 后缀不是隐式平台约束），把 Linux-only 的 godbus/dbus + notifier 链进每个 Windows 构建。desktop/go.mod replace 指向本地副本。实测本地 exe 13,273,600→12,909,568 B（-364,032 B），二进制内 godbus 串 506→0。见 third_party/systray/PATCH.md。
- 前端：TaskList 排序预计算每行 key + Intl.Collator 模块单例 + failed 置顶并入同一次比较（原来两趟 sort）；顺序指纹缓存——progress tick 不再每 250ms 全表 O(n log n) localeCompare 重排（不变序时映射旧序到新对象，行照常刷新）；分组一次遍历同时产出各组 exts（原来每组各扫全表）；列宽/侧栏宽度 localStorage 防抖 300ms（拖拽不再逐帧写盘）；keydown 全局监听改 ref 单次订阅（原来每次 tick 重挂）；formatWhen 的"今天"按分钟缓存。
- 验证：go vet 全过；engine go test 全绿；desktop go test 除基线已知 TempDir 清理竞态（轮换 victim，单测隔离全过）外全绿；tsc 0 错；vitest 22/22；扩展 node 测试未动本轮无改。新 exe 已构建并重启（本地实测 PID 2084，日志干净）。
- 有意不做并记录原因：ReserveSize 自适应（先吃默认值 80% 收益）；debugf 环境变量缓存（与测试 t.Setenv 生命周期冲突）；saveTasks 紧凑 JSON（Indent 是诊断可读性、故意的，且突发写量受 maxConcurrent 限界）；任务列表虚拟化（当前规模用不上）；Sidebar memo（内联 lambda 会抵消收益）；探测完成校验彻底异步化（保持语义不变，只把 Stat 挪出锁）；前端 gzip 资产、更新检查延迟（收益/成本比不足）。

### 50. 引擎韧性：CDN"红/绿窗口"不再杀死整个任务（UNRELEASED）
- 背景（实测）：uploadhaven 这类站的 CDN 存在分钟级的"红/绿"状态摆动——红色窗口内**任何请求**（不分大小、区域、并发、请求头）一律 404，几分钟后自愈；绿色窗口一切正常（单线程 168 KB/s、8 路并发 Range 全 206）。而引擎把任何一个 worker 收到的任何一个 404 判为永久性失败：一次瞬时故障就杀死整个任务，错误还误导（"链接不存在，请检查地址"——地址明明是对的，字节都下了一半）。用户的 245MB 任务三次分别死在 341KB/390KB/81KB，全是撞窗。
- 引擎层：`statusError` 增加 `PostProgress` 标记——session 已有字节落盘（含断点恢复的 Written）之后再收到的 404 视为暂时性故障，可重试；探测路径的 404 永不带标记，"链接写错"依旧秒报错。`fetchSegment` 为这种 404 设**独立且不被 `errSegmentIncomplete` 重置**的预算（5 次）——否则红绿摆动 + 预算被成功块重置 = 无限重试。新增 `IsNotFound`/`RecoverableNotFound`/`ResumableProgress` 供上层判定。
- manager 层：任务因"有进展的 404"失败时不再标 failed，而是在 `run()` 内部以 20s/40s/60s 退避自动重放整个会话（从 sidecar 断点续传），任务全程保持"下载中"（计划下载不会提前记为"介入"，暂停/取消/更新冻结能立刻打断等待）；重试等待期间任务行显示"服务器临时故障（404），N 秒后自动重试（第 i/3 次）"（`TaskView` 新增 `Notice` 字段 + 任务行一处渲染）；重试耗尽才真正失败，终局文案"服务器持续返回 404，链接可能已失效或正被限流 · 请稍后重试，或重新获取下载链接"（不经 humanError 的 404 启发式，避免"链接不存在"的误导）。
- testsrv：新增 `FlapAfter/FlapNotFound/FlapPeriod` 故障模式（放在 `writeResponse`，keep-alive 复用连接也计数），可精确模拟红/绿窗口，`SetFlap` 允许测试中途"治好"服务器。
- 测试：engine 5 个新用例（扛过窗口且逐字节正确 / FlapPeriod 下有限终止 / 404 预算不被成功块重置 / 恢复判定证据链 / isRetryable 表）、manager 7 个新用例（flap 重放成功 / 中途窗口恢复后完成 / 耗尽文案 / 首次 404 快速失败 / Pause 与 Cancel 立即打断 / 更新冻结不重启）。
- 验收：go vet 三处干净；engine、desktop 全量测试通过；GOOS=linux 可编译；tsc 0 错、vitest 26/26。

### 51. 全量审计修复第 1 批：引擎数据正确性 + 前端/拖放（UNRELEASED）
- 审计背景：7 个并行只读审计（完整报告见工作区 `audit-2026-10-18.md`，去重后 73 条）。本批落地引擎 5 条 + testsrv 测试盲区 + 前端 13 条 + 拖放读文件。
- 断电全 0 文件（P0）：sidecar 落盘前先 fsync `.part`，新增 `syncedHigh` 已确认落盘高水位；`effectiveAvailable` 不再信任预分配后的文件大小，改为高水位（再与真实文件大小取 min）；被截短的 segment 标记未完成。为什么改：分段路径一开始就把 `.part` 预分配到完整大小，所以"文件大小=完整大小"从第一秒成立，断电后 sidecar 声称写完、盘上全是 0，任何长度校验都拦不住——审计在仓库外跑通了"resume → publish 4MiB 全 0 文件"的完整链条。
- 并发 sidecar（P1）：`session.saveMu` 串行化所有 flush；临时文件名改为 `<path>.vkmdownload-<pid>-<counter>.tmp`、写后隐藏、失败即删。为什么改：多 worker 在锁外并发写同一个固定 `.tmp`，审计实测 24000 次调用 23128 次失败、518 次读到非法 JSON——断点续传与 404 恢复判据会被静默破坏，且失败会因水位不推进而自我放大。
- 未知长度溢出（P1）：无界 segment 的 End 改为 `MaxInt64-1`，`Length()` 对边界饱和。为什么改：`End-Start+1` 溢出成负数后 `Written` 被写成 `-2^63` 落盘，`progressed()`/`ResumableProgress` 恒 false，404 韧性在 chunked/无 Content-Length 资源上完全失效（审计 A/B 实测）。
- 续传证据收紧（P2）：`ResumableProgress` 要求 sidecar 身份匹配（TargetPath/TempPath）且 `.part` 存在非空；恢复的分段表必须精确铺满 `[0,size)`（无洞/无重叠/不越界），否则丢弃重下。为什么改：残留 sidecar 会把真正失效的链接拖进两分钟恢复循环；带洞分段表会产出"长度正确、中间空洞"的文件。
- sidecar 卫生（P3）：损坏 / 版本不符 / 不匹配 / 不支持 Range 的 sidecar 一律删除并记日志（不再永久滞留、每次 resume 重读）；publish 删 sidecar 失败记日志。
- testsrv：`NoContentLength` 下 HEAD 不再宣布 `Content-Length`。为什么改：HEAD 说知道长度、GET 却 chunked 的不一致，让引擎的未知长度路径长期零测试覆盖。
- 前端（13 项）：剪贴板按设置行事（不再无视 `clipboardConfirm`/`clipboardAutoStart`）；completed 的"整理失败"消息可见；新增 `ErrorBoundary` 兜底白屏 + 设置页 `organizeGroups` null 防御；Esc 捕获阶段停止传播；`toDialogText` 超长单行按词边界截断；侧栏拖拽先 flush 挂起帧并在卸载时清理；列宽把手补齐第 5 个；行内确认框不再冒泡选中整行；URL 校验改用 `new URL()` + 长度上限；"正在应用"失败时复位并提示；空列表提示改为真实行为；`format` 子字节值不再渲染 `undefined`；30ms 聚焦定时器清理；删除 `planRunning` 死绑定。
- 拖放读文件：新增 `droppedText`——≤256KiB 的文本文件按内容交给前端识别（`.url`/`.txt` 里的链接现在真的能拖入），其余回退为路径，保持原行为。
- 测试：engine 新增 4 个测试文件、8 个用例（未知长度 A/B、2400 次并发 save、断电模拟 resume、损坏/版本不符/带洞 sidecar）；前端 30/30（新增 ErrorBoundary、超长单行、子字节格式化用例）。验收：根 `vet`/`test`/`-race`、desktop `vet`/`test`、`GOOS=linux vet` 全干净；`tsc` 0 错；vitest 30/30。

## TODO (user requested, work later)

- Detached ask-location popup WITHOUT v3 migration (backup plan).
  Idea: tiny companion program (Go stdlib + native dialog, tens of KB) that
  shows URL/dir/filename/connections and POSTs back to the main app's
  loopback API. Main app stays in tray; helper launched on ask arrivals.
  Pros: detached small window now, no migration. Cons: second component to
  maintain, plain styling, edge cases (e.g. helper with no app running).
  Requested 2026-10-15 as an option. Not started.


- Detached ask-location dialog in its own small window (no main window).
  WHY v3: user wants only the small placement dialog to pop when a browser
  download arrives, without the whole main window coming along. Wails v2 is
  single-window so the modal must live in the main window (bring-to-front,
  CHANGES #11, is the current ceiling); only Wails v3 has multi-window.
  STATUS (2026-10-15): parked on purpose. v3 is still beta (APIs unstable),
  and migration is rewrite-scale (window model, JS bridge, bindings,
  packaging all change; manager/tray/update/clipboard/loopback/React all
  need porting). Frameworks reviewed: Electron (needs Go engine as sidecar
  + huge bundle), Tauri (Rust rewrite), Fyne/walk (React UI rewrite).
  Verdict: stay on v2 until v3 goes stable - Go code moves over almost
  untouched then. Revisit only when the small window becomes a must.


- Multi-select + multi-edit tasks in the list. Bulk start/stop/remove DONE (2026-10-15, CHANGES #12). Batch field editing (connections/destination) still open.

## 2026-10-14 — 0.2.4（已发布）
- 见 `release-notes-0.2.4.md`。
