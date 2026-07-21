# Plane × Looper 本地部署与派活

这套流程让每位研发只把任务派给自己电脑上的 Looper。Plane 保存唯一执行归属；飞书只负责通知，不读取飞书回复。

## 研发首次接入

1. 安装同一版本的 Looper，确认项目里没有旧版 daemon 或旧启动项仍在运行。
2. 配置 Plane provider（`workspace`、`projectId`、`tokenEnv`）并加入团队 loopernet。
3. 绑定当前 Plane 身份和本机 Node：

   ```bash
   looper plane link <provider-id>
   ```

   命令会生成仅当前用户可读的 Ed25519 私钥，并输出 `bindingId`。私钥不会上传。
4. 把 `bindingId` 发给 Plane Project Admin。管理员执行：

   ```bash
   looper plane approve <binding-id> <provider-id>
   ```

   只有明确允许离线排队时才追加 `--allow-offline-queue`。
5. 管理员完成项目角色与 strict rollout 配置后，研发执行：

   ```bash
   looper plane enable <provider-id>
   looper daemon restart
   ```

## Project Admin 首次启用

所有准备接入该项目的 Looper 都升级、旧进程均停止、Node 在线并完成 binding 审批后，设置产品、设计、QA 负责人并激活项目：

```bash
looper plane setup \
  <product-plane-member-id> \
  <design-plane-member-id> \
  <qa-plane-member-id> \
  <provider-id> \
  --checklist-revision 1
```

研发负责人不单独配置，始终等于该 work item 的 Looper owner。任一已批准 Node 离线、未上报 strict capability 或角色成员不在项目中，激活会 fail closed。

## 日常派活

打开 Plane work item 的 Looper 面板，确认卡片底部显示正确的 owner、Node 和在线状态，然后点击“派给我的 Looper”。V1 不提供跨 owner 目标选择，也不允许管理员替别人派活。

自动模式依次执行：需求调研 → 多角色决策 → 中文技术方案 → owner 审批 → Worker 实现 → PR/QA。产品与设计问题在 Plane 对应入口回答；飞书仅发通知。较大的产品需求会要求正式 product spec，小问题允许一句快速决策。

## 停止与恢复

Plane 的“请求停止”不是远程强杀。运行中的 agent 仍由原 owner 本地停止并由原 Node 确认进程已经退出；Node 丢失时保持等待，不自动改派。确认停止后，owner 填写原因并 release，其他研发才可把同一 work item 派给自己的 Looper。

loopernet 暂时不可用时，Plane 仍展示持久化的 owner、阶段和产物，只把实时在线状态降级为 `unavailable`。
