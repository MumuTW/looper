# Prompt — 帮我配置并启动 looper(飞书 HITL)

把下面这段整段发给你的 coding agent(Claude Code / codex / …),在**解压后的这个目录里**运行它。

---

你在帮我配置并启动 **looper** —— 一个自主开发 agent 的守护进程 —— 以及它的「人在环(HITL)」飞书集成。**交互式**地陪我做:任何对外(GitHub / 飞书)或不可逆的操作前先跟我确认。

## 这个包里有什么
- `config.hitl.example.json` —— 带占位符的配置模板。
- `hitl.env` —— 团队**共享**的飞书 / Worker secret(已填好;**不要打印、不要写进 git、不要贴进聊天**)。
- `GUIDE-hitl-setup.md` —— 参考文档,先读它。

## 按这个来
1. **查前置条件**,缺什么告诉我:
   - `looperd` 和 `looper` 在 PATH 上(没有就问我路径 / 怎么装或编译)。
   - 我的 coding agent 二进制(`codex` 或 `claude`)已安装且**已登录/授权**(跑一下确认能用)。
   - 我要 looper 处理的那个 GitHub 仓库,本地已 clone。
2. **收集我的设置**(问我,别猜):
   - GitHub 仓库(`owner/repo`)+ 本地 clone 的**绝对路径**。
   - 我的飞书**群 chat id**(`oc_...`)。
   - **我自己的 open_id 不用手动找** —— 第 5 步会用 `looper login` 自动抓(浏览器授权一下,写进配置的 `owner` 和 `mentionOpenIds`)。
   - `productOwner` 和 `qa` 的 open_id(**团队统一**,分发方会给你 —— 产品负责人 / QA 的 `ou_...`;它们已在模板里留了 `REPLACE_...` 占位)。
   - 我的 coding agent 二进制的绝对路径,以及用 `codex` 还是 `opencode`(见下方「后端」)。
   - looper 数据/日志放哪(默认 `~/.looper`)。
3. **写我的配置**:把 `config.hitl.example.json` 复制到 `~/.looper/config.json`,把所有 `REPLACE_...` / `/ABSOLUTE/...` / `OWNER/REPO` / `FILLED_BY_looper_login_...` 占位符替换成上面的值(`owner` 那个先留着,下一步 `looper login` 填)。改完把最终配置**给我看一遍**确认(它没有 secret,可以直接展示)。
   - **后端**:模板默认 `codex`(`-c model=gpt-5.4 …`)。若我用 `opencode`,把 `agent.vendor` 改 `opencode`、`command` 指向 opencode 二进制、`model` 用 `openai/gpt-5.4`、`args` 换成 `["--pure","--dangerously-skip-permissions"]`(codex 的 `-c/-s` 参数在 opencode 里含义不同,别混用)。
4. **加载共享 secret**:`source` 这个包里的 `hitl.env`。**不要**把 secret 的值打印出来,也**不要**抄进配置文件(配置里只放变量名)。
5. **登录飞书拿我的 open_id**(自动,先 `source hitl.env` 让 app 凭证进 shell):
   ```sh
   source <包路径>/hitl.env                       # app_id/secret 等进当前 shell
   looper login --config ~/.looper/config.json    # 开浏览器授权,自动把我的 open_id 写进 owner + mentionOpenIds
   ```
   （前提:共享飞书 app 里已把 `http://127.0.0.1:53682/callback` 加进「重定向 URL 白名单」—— 这是**分发方一次性**做的 app 级设置,你不用管;若报 `20029 重定向 URL 有误` 就找分发方补这条。）
6. **装成常驻服务并启动**(这样合盖 / 重启 / 崩了都会自动拉起 —— 别用 `nohup` 裸后台):
   ```sh
   looper daemon install
   looper daemon start --config ~/.looper/config.json --daemon-restart-policy on-failure
   ```
   确认活着:`looper daemon status --config ~/.looper/config.json`。(密钥来自上面 source 的 `hitl.env`,会被写进 launchd 服务,重启后仍在。)
7. **冒烟**(先问我):在我的仓库建一个带 `looper:plan` 标签的小 issue,确认 looper 接住、并在(有歧义时)往我的飞书群发一张决策卡 @我。

## 护栏
- 永远不要把 `hitl.env` 或配置提交进 git;永远不要把 secret 贴进聊天。
- 建 GitHub issue/PR、往飞书发东西之前**先问我**。
- 前置条件缺了就停下来告诉我,别假装装好了。
