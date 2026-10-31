# 参与开发

## 环境

- Go 1.22+，无第三方依赖，不需要 `go mod download`
- Python 3.10+（benchmark 用，标准库即可跑；画图需要 matplotlib）

## 提交前检查

```bash
go vet ./...
go test ./...
go test -run='^$' -fuzz FuzzPruneJSON -fuzztime 30s ./internal/compress
python bench/gen_tasks.py
# 离线端到端：./bin/tokencut --mock-upstream 起来后跑 bench/run.py --dry-run --limit 8
```

## 几条约定

1. fail-open 是底线：压缩路径上的任何代码，出错时必须回退到转发原始请求，
   并补一条对应测试。
2. 改 jsonpruner 必跑 fuzz，输出合法性和"不比输入大"是硬不变量。
3. 数字只能来自 `go test -bench` 或 bench harness 实测，写法见
   bench/benchmark-methodology.md。
4. 加依赖前先说明标准库为什么做不到。
5. 注释解释为什么这么写，而不是复述代码在干什么。

## 报告压缩质量问题

开 issue 时附：payload 形态（脱敏）、所用档位、`x-tokencut-*` 响应头、
模型输出里能观察到的错误。有复现脚本最好。
