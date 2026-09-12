# 前端测试套件

本目录是控制台的**前端验证基础设施**。此前它们只存在于开发机的临时目录
（`D:\tmp`），仓库里一个都没有 —— 也就是说：别人 clone 下来**无法跑任何
前端验证**，也无法复现交付报告里的结论。已收入版本库。

## 两个入口

```bash
# 静态套件（18 个）：不启动服务，直接读源码做断言。快，无需浏览器。
node tests/frontend/run_frontend_suites.js

# 浏览器 E2E（14 个）：需要真实运行的控制台 + Chrome。
node tests/frontend/run_e2e.js http://127.0.0.1:18080/ui
```

## 目录内容

| 类别 | 说明 |
|---|---|
| `run_*.js` | 两个 runner（串行执行 + 端口隔离 + 产物门禁） |
| `gen_*.js` | 16 个断言发生器：读源码 → 写出对应的 `*_gen.js` |
| `*_gen.js` | 发生器产出的**可执行断言**（可被 generator 重新生成） |
| `*_e2e.js` | 独立浏览器端到端套件（真实 Chrome + CDP） |
| `flash_test.js` | 抗闪白：采样加载阶段的 `data-theme`，防首屏闪烁 |
| `p4_badge_check.js` | 徽章底色必须**跟随主题**（不是写死的颜色） |
| `contrast_e2e.js` | 两套主题下各组件的 WCAG AA 对比度 |
| `panel_content_e2e.js` | 逐面板断言**内容真的渲染**（不是只有结构） |

## 设计上的三条硬规则

1. **generator 必须成功**，否则产物陈旧会假装绿（runner 有门禁）
2. **断言要与数据对照**，不能只断言"某字符串出现过"
   （本项目三次栽在这里：颜色字符串、徽章存在性、错误码文本）
3. **测量工具本身必须先被验证** —— 变异测试是标准做法：
   把已知会破坏功能的改动注入副本，确认对应套件**真的会红**

## 变异扫描（验证"守卫是否真的有效"）

```bash
node mutation_sweep.js --self-test   # 只自检扫描器本身
node mutation_sweep.js static        # 静态组（快，不需要浏览器）
node mutation_sweep.js e2e           # E2E 组（需要 Chrome + 可访问的实例）
```

当前状态：**15 个变异全部被抓住，0 存活**。

判读结果时注意——**"存活"有三种含义，必须人工区分**：

| 含义 | 处置 |
|---|---|
| A. 测试真的有洞 | 补断言 |
| B. 变异无效（改了但行为没变） | 换一个真正影响行为的注入 |
| C. 环境不可观测（如新实例日志为空） | **不能算缺陷** |

三种都真实发生过，混为一谈会得出错误结论。

## 已知的环境限制

### `go test -race` —— 上游缺陷，换编译器也解决不了

我此前记录为"因无 C 编译器"，**联网查证后发现那个归因是错的**。
真实原因是 Go 运行时在 Windows 上的 TSan 缺陷：它算出的影子内存基址
**超出内核允许的用户态虚拟地址上限**。实测：

```
ThreadSanitizer failed to allocate 0x000004200000 (69206016) bytes
at 0x100ec90b50000 (error code: 87)
```

上游同形态报告（均未修复）：

- [golang/go#46099](https://github.com/golang/go/issues/46099) —— 同样 `error code: 87`，
  状态 closed 但标签为 `FrozenDueToAge`（长期无进展被冻结，**非修复**）
- [golang/go#28497](https://github.com/golang/go/issues/28497) —— **仍 open**
- [golang/go#22553](https://github.com/golang/go/issues/22553) —— **仍 open**
- [tailscale/tailscale#4926](https://github.com/tailscale/tailscale/issues/4926)
  给出了准确诊断：TSan 要用的基址"higher than the maximum virtual address
  permitted by the kernel for user-mode VM allocation requests"

**所以装 MinGW 不能解决**（问题在 TSan 的地址计算，不在 C 编译器）。
并发正确性由 `internal/pool/concurrency_norace_test.go` 的不变式断言部分覆盖 ——
它能抓到竞争导致的**可见后果**，但**不等价于 race detector**。

### 其它

- 需要真实凭证的用例（codearts 的 `*Live`）会自动 skip。
  签名**实现与官方文档的一致性**已由 `verify_sign_against_spec.js` 覆盖，
  但那不能替代真实凭证的端到端验证。

**浏览器路径不再是限制**：各套件通过 `chrome_path.js` 解析，
顺序为 `CHROME_PATH` → 常见安装位置（Chrome/Edge，含 macOS/Linux）→ `PATH`。
找不到时会**明确报错并列出试过的位置**。若 Chrome 装在非标准位置：

```bash
CHROME_PATH=/path/to/chrome node run_e2e.js http://127.0.0.1:18080/ui
```

该解析器自身有测试（`test_chrome_resolver.js`，静态套件的一部分），
断言覆盖链真的生效、且找不到时不会静默返回空串。
