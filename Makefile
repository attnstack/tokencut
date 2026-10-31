# tokencut — build & verify.
# Windows 用户：Git Bash 下逐条执行 target 里的命令即可（README 有清单）。

GO ?= go
PYTHON ?= python3

.PHONY: build test vet fuzz demo bench-gen bench-dry bench-real plot clean

build:
	$(GO) build -o bin/tokencut ./cmd/tokencut

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# 60s fuzz run (CI uses 15s); requires Go 1.18+
fuzz:
	$(GO) test -run='^$$' -fuzz FuzzPruneJSON -fuzztime 60s ./internal/compress

# full offline demo: mock upstream + dry-run benchmark (no API key needed)
demo: build
	./bin/tokencut --mock-upstream &
	sleep 1
	cd bench && $(PYTHON) gen_tasks.py && $(PYTHON) run.py --dry-run --limit 25
	$(PYTHON) bench/judge.py
	kill %1 2>/dev/null || true

bench-gen:
	cd bench && $(PYTHON) gen_tasks.py

# offline pipeline benchmark (mock reference solver; NOT model quality)
bench-dry: bench-gen build
	./bin/tokencut --mock-upstream &
	sleep 1
	cd bench && $(PYTHON) run.py --dry-run
	kill %1 2>/dev/null || true
	$(PYTHON) bench/judge.py
	$(PYTHON) bench/plot.py

# real benchmark on a local Ollama model (proven path, zero API cost):
#   printf 'FROM qwen2.5:7b\nPARAMETER num_ctx 16384\n' > /tmp/Modelfile
#   ollama create tokencut-eval -f /tmp/Modelfile
bench-real-ollama: bench-gen build
	OLLAMA_HOST=127.0.0.1:11434 ollama list >/dev/null  # assert ollama is up
	./bin/tokencut & export TOKENCUT_UPSTREAM=http://localhost:11434
	sleep 1
	cd bench && $(PYTHON) run.py --model tokencut-eval --runs 1 --timeout 300
	$(PYTHON) bench/judge.py --judge-model tokencut-eval --api-base http://127.0.0.1:8787
	$(PYTHON) bench/plot.py

# real benchmark against a hosted OpenAI-compatible endpoint (needs budget,
# see bench/benchmark-methodology.md §4; smoke with --limit 10 first)
bench-real: bench-gen build
	./bin/tokencut &
	sleep 1
	cd bench && $(PYTHON) run.py --proxy http://127.0.0.1:8787 \
		--direct https://api.openai.com --model gpt-4o-mini --api-key $${OPENAI_API_KEY}
	kill %1 2>/dev/null || true
	$(PYTHON) bench/judge.py --judge-model gpt-4o-mini --api-base http://127.0.0.1:8787 --api-key $${OPENAI_API_KEY}
	$(PYTHON) bench/plot.py

clean:
	rm -rf bin bench/pareto.png bench/results*.jsonl bench/summary.json
