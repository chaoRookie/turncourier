<!--
Do not include QQ Mail authorization codes, tokens, passwords, real email content, Agent session transcripts or local absolute paths.
Report security vulnerabilities through private vulnerability reporting as described in SECURITY.md, not in a pull request.

请勿包含 QQ 邮箱授权码、令牌、密码、真实邮件内容、Agent 会话内容或本机绝对路径。
安全漏洞请按 SECURITY.md 通过私密漏洞报告提交，不要通过 PR 公开。
-->

## Summary / 变更摘要

<!-- What changed and why. / 改了什么，为什么改。 -->

## Related issue / 关联 issue

<!-- For example: Closes #123. Write "None" for small fixes. / 例如 Closes #123；小修复可写“无”。 -->

## Verification / 验证

<!-- Tick only what you ran on the final version of this change. / 只勾选针对本变更最终版本实际运行过的项目。 -->

- [ ] `make check`
- [ ] `make security`
- [ ] `make workflows`
- [ ] `make build`
- [ ] Manual smoke test / 手工冒烟测试：`./dist/turncourier help`, `./dist/turncourier version`, `./dist/turncourier doctor --json`

Not run, and why / 未运行的项目及原因：

## Documentation / 文档

- [ ] Documentation updated, or no update needed (`README.md`, `README.zh-CN.md`, `CHANGELOG.md`, `docs/`) / 文档已更新或无需更新
- [ ] Directory trees in `README.md` and `README.zh-CN.md` match the actual files / 中英文 README 的树形图与实际文件一致

## Data and credentials / 数据与凭据

- [ ] No credentials, tokens or authorization codes / 不含凭据、令牌或授权码
- [ ] No real email content, Agent session transcripts or local absolute paths; tests and examples use synthetic data / 不含真实邮件内容、Agent 会话内容或本机绝对路径；测试和示例使用合成数据
- [ ] Tests run offline and do not call a real mailbox or model / 测试离线运行，不调用真实邮箱或模型
