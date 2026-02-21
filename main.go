package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

type TestRequest struct {
	BaseURL     string   `json:"baseUrl"`
	Keys        []string `json:"keys"`
	KeysText    string   `json:"keysText"`
	Concurrency int      `json:"concurrency"`
	TimeoutSec  int      `json:"timeoutSec"`
}

type KeyResult struct {
	Index      int    `json:"index"`
	KeyRaw     string `json:"keyRaw,omitempty"`
	KeyMasked  string `json:"keyMasked"`
	StatusCode int    `json:"statusCode"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	LatencyMs  int64  `json:"latencyMs"`
}

func maskKey(k string) string {
	if len(k) <= 20 {
		return k
	}
	return k[:18] + "..." + k[len(k)-6:]
}

func parseKeys(req TestRequest) []string {
	m := map[string]struct{}{}
	out := make([]string, 0)
	push := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" {
			return
		}
		if _, ok := m[k]; ok {
			return
		}
		m[k] = struct{}{}
		out = append(out, k)
	}
	for _, k := range req.Keys {
		push(k)
	}
	if req.KeysText != "" {
		s := bufio.NewScanner(strings.NewReader(req.KeysText))
		for s.Scan() {
			push(s.Text())
		}
	}
	return out
}

func testOne(ctx context.Context, client *http.Client, baseURL, key string, idx int) KeyResult {
	url := strings.TrimRight(baseURL, "/") + "/models"
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return KeyResult{Index: idx, KeyRaw: key, KeyMasked: maskKey(key), StatusCode: 0, OK: false, Error: err.Error(), LatencyMs: lat}
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	msg := ""
	if resp.StatusCode >= 400 {
		t := strings.TrimSpace(buf.String())
		if len(t) > 160 {
			t = t[:160]
		}
		msg = t
	}
	return KeyResult{Index: idx, KeyRaw: key, KeyMasked: maskKey(key), StatusCode: resp.StatusCode, OK: resp.StatusCode == 200, Error: msg, LatencyMs: lat}
}

func normalizeReq(req *TestRequest) {
	if req.BaseURL == "" {
		req.BaseURL = "https://api.openai.com/v1"
	}
	if req.Concurrency <= 0 {
		req.Concurrency = 200
	}
	if req.Concurrency > 2000 {
		req.Concurrency = 2000
	}
	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 8
	}
	if req.TimeoutSec > 60 {
		req.TimeoutSec = 60
	}
}

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true, "ts": time.Now().Unix()})
	})

	// Standard batch API
	r.POST("/api/test-keys", func(c *gin.Context) {
		var req TestRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		normalizeReq(&req)
		keys := parseKeys(req)
		if len(keys) == 0 {
			c.JSON(400, gin.H{"error": "no keys provided"})
			return
		}

		client := &http.Client{Timeout: time.Duration(req.TimeoutSec) * time.Second}
		ctx := c.Request.Context()

		jobs := make(chan struct {
			idx int
			key string
		}, len(keys))
		results := make([]KeyResult, 0, len(keys))
		var mu sync.Mutex
		var done int64

		worker := func() {
			for j := range jobs {
				res := testOne(ctx, client, req.BaseURL, j.key, j.idx)
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				atomic.AddInt64(&done, 1)
			}
		}

		for i := 0; i < req.Concurrency; i++ {
			go worker()
		}
		for i, k := range keys {
			jobs <- struct {
				idx int
				key string
			}{idx: i + 1, key: k}
		}
		close(jobs)

		for atomic.LoadInt64(&done) < int64(len(keys)) {
			time.Sleep(15 * time.Millisecond)
		}
		sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })

		okCount := 0
		for _, x := range results {
			if x.OK {
				okCount++
			}
		}
		c.JSON(200, gin.H{"total": len(results), "ok": okCount, "failed": len(results) - okCount, "results": results})
	})

	// Streaming batch API (ndjson events)
	r.POST("/api/test-keys-stream", func(c *gin.Context) {
		var req TestRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		normalizeReq(&req)
		keys := parseKeys(req)
		if len(keys) == 0 {
			c.JSON(400, gin.H{"error": "no keys provided"})
			return
		}

		w := c.Writer
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			c.JSON(500, gin.H{"error": "streaming unsupported"})
			return
		}

		encoder := json.NewEncoder(w)
		send := func(v any) bool {
			if err := encoder.Encode(v); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		if !send(gin.H{"type": "start", "total": len(keys), "concurrency": req.Concurrency, "timeoutSec": req.TimeoutSec}) {
			return
		}

		ctx := c.Request.Context()
		client := &http.Client{Timeout: time.Duration(req.TimeoutSec) * time.Second}
		jobs := make(chan struct {
			idx int
			key string
		}, len(keys))
		resultsCh := make(chan KeyResult, req.Concurrency*2)

		var wg sync.WaitGroup
		for i := 0; i < req.Concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					select {
					case <-ctx.Done():
						return
					default:
					}
					resultsCh <- testOne(ctx, client, req.BaseURL, j.key, j.idx)
				}
			}()
		}

		for i, k := range keys {
			jobs <- struct {
				idx int
				key string
			}{idx: i + 1, key: k}
		}
		close(jobs)

		go func() {
			wg.Wait()
			close(resultsCh)
		}()

		okCount := 0
		failCount := 0
		for res := range resultsCh {
			if res.OK {
				okCount++
			} else {
				failCount++
			}
			if !send(gin.H{"type": "result", "data": res, "ok": okCount, "failed": failCount, "done": okCount + failCount, "total": len(keys)}) {
				return
			}
		}

		_ = send(gin.H{"type": "summary", "total": len(keys), "ok": okCount, "failed": failCount})
	})

	r.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(200, pageHTML)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "18080"
	}
	fmt.Printf("listening on %s\n", port)
	_ = r.Run(":" + port)
}

const pageHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Key Batch Tester</title>
  <style>
    :root { color-scheme: dark; }
    body { font-family: Inter, system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; background:#0b1220; color:#e5e7eb; }
    .wrap { max-width: 1200px; margin: 24px auto; padding: 0 16px; }
    .card { background:#111827; border:1px solid #1f2937; border-radius:12px; padding:16px; margin-bottom:16px; }
    h1 { margin: 0 0 12px 0; font-size: 22px; }
    .grid { display:grid; grid-template-columns: 2fr 1fr 1fr 1fr; gap:10px; }
    input, textarea, button { width:100%; box-sizing:border-box; border-radius:8px; border:1px solid #374151; background:#0f172a; color:#e5e7eb; padding:10px; }
    textarea { min-height:220px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    textarea.dragover { border-color:#60a5fa; box-shadow:0 0 0 2px rgba(96,165,250,.35) inset; }
    button { cursor:pointer; background:#2563eb; border:none; }
    button.secondary { background:#374151; }
    .row { display:flex; gap:10px; }
    .stat { display:flex; gap:18px; font-size:14px; margin:8px 0; }
    .pill { background:#0f172a; border:1px solid #374151; border-radius:999px; padding:4px 10px; }
    table { width:100%; border-collapse: collapse; font-size:13px; }
    th, td { border-bottom:1px solid #1f2937; padding:8px; text-align:left; vertical-align:top; }
    .ok { color:#22c55e; }
    .bad { color:#ef4444; }
    .muted { color:#9ca3af; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="card">
      <h1>Key Batch Tester (Go + Gin)</h1>
      <div class="grid">
        <div><label>Base URL</label><input id="baseUrl" value="https://api.openai.com/v1" /></div>
        <div><label>Concurrency</label><input id="concurrency" type="number" value="200" min="1" max="2000" /></div>
        <div><label>Timeout (sec)</label><input id="timeoutSec" type="number" value="8" min="1" max="60" /></div>
        <div style="display:flex;align-items:end;"><button id="startBtn">开始测试</button></div>
      </div>
      <div style="margin-top:10px;"><label>Keys（每行一条）</label><textarea id="keysText" placeholder="sk-...\nsk-...\nsk-..."></textarea></div>
      <div class="row" style="margin-top:10px;">
        <button class="secondary" id="pauseBtn">暂停</button>
        <button class="secondary" id="stopBtn">停止</button>
        <button class="secondary" id="importBtn">导入TXT</button>
        <button class="secondary" id="exportOkBtn">导出成功</button>
        <button class="secondary" id="clearBtn">清空结果</button>
      </div>
      <input id="fileInput" type="file" accept=".txt,text/plain" style="display:none" />
    </div>

    <div class="card">
      <div class="stat">
        <span class="pill">总数: <b id="total">0</b></span>
        <span class="pill">完成: <b id="done">0</b></span>
        <span class="pill ok">可用: <b id="ok">0</b></span>
        <span class="pill bad">失败: <b id="fail">0</b></span>
        <span class="pill">状态: <b id="status" class="muted">idle</b></span>
      </div>
      <div style="overflow:auto; max-height: 60vh;">
        <table>
          <thead><tr><th>#</th><th>Key</th><th>HTTP</th><th>耗时(ms)</th><th>结果</th><th>错误摘要</th></tr></thead>
          <tbody id="tbody"></tbody>
        </table>
      </div>
    </div>
  </div>

<script>
const $ = (id) => document.getElementById(id);
const tbody = $("tbody");
let running = false;
let paused = false;
let stopped = false;
let activeReader = null;
const okRawKeys = [];

function esc(s){ return (s||"").replace(/[&<>\"]/g,m=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;"}[m])); }
function setStatus(t){ $("status").textContent=t; }
function resetStats(){ $("total").textContent="0"; $("done").textContent="0"; $("ok").textContent="0"; $("fail").textContent="0"; }
function sleep(ms){ return new Promise(r=>setTimeout(r,ms)); }

$("clearBtn").onclick = () => { tbody.innerHTML = ""; okRawKeys.length = 0; resetStats(); setStatus("idle"); };

$("pauseBtn").onclick = () => {
  if (!running) return;
  paused = !paused;
  $("pauseBtn").textContent = paused ? "继续" : "暂停";
  setStatus(paused ? "paused" : "running");
};

$("stopBtn").onclick = () => {
  stopped = true;
  paused = false;
  $("pauseBtn").textContent = "暂停";
  if (activeReader) {
    try { activeReader.cancel(); } catch {}
  }
  setStatus("stopped");
};

$("importBtn").onclick = () => $("fileInput").click();
async function appendFromTxtFile(f){
  if (!f) return;
  const name = (f.name || '').toLowerCase();
  if (!(name.endsWith('.txt') || f.type === 'text/plain' || f.type === '')) {
    alert('只支持 .txt 文件');
    return;
  }
  const text = await f.text();
  const prev = $("keysText").value.trim();
  $("keysText").value = prev ? (prev + "\n" + text) : text;
}

$("fileInput").addEventListener('change', async (e) => {
  const f = e.target.files && e.target.files[0];
  await appendFromTxtFile(f);
  e.target.value = "";
});

const keysArea = $("keysText");
keysArea.addEventListener('dragover', (e) => {
  e.preventDefault();
  keysArea.classList.add('dragover');
});
keysArea.addEventListener('dragleave', () => {
  keysArea.classList.remove('dragover');
});
keysArea.addEventListener('drop', async (e) => {
  e.preventDefault();
  keysArea.classList.remove('dragover');
  const f = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
  await appendFromTxtFile(f);
});

$("exportOkBtn").onclick = () => {
  if (!okRawKeys.length) { alert("暂无成功 key 可导出"); return; }
  const uniq = Array.from(new Set(okRawKeys));
  const blob = new Blob([uniq.join('\n') + '\n'], {type:'text/plain;charset=utf-8'});
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = 'ok-keys-' + Date.now() + '.txt';
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
};

$("startBtn").onclick = async () => {
  if (running) return;
  const payload = {
    baseUrl: $("baseUrl").value.trim(),
    concurrency: Number($("concurrency").value || 200),
    timeoutSec: Number($("timeoutSec").value || 8),
    keysText: $("keysText").value
  };
  if (!payload.keysText.trim()) { alert("请先粘贴 key（每行一条）"); return; }

  running = true;
  paused = false;
  stopped = false;
  $("pauseBtn").textContent = "暂停";
  okRawKeys.length = 0;
  setStatus("running");
  tbody.innerHTML = "";
  resetStats();

  try {
    const res = await fetch('/api/test-keys-stream', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify(payload)
    });

    if (!res.ok || !res.body) {
      const t = await res.text();
      setStatus('error');
      alert('请求失败: ' + t);
      running = false;
      return;
    }

    const reader = res.body.getReader();
    activeReader = reader;
    const decoder = new TextDecoder();
    let buffer = '';

    while (true) {
      if (stopped) break;
      if (paused) { await sleep(120); continue; }
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream:true });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';
      for (const line of lines) {
        if (!line.trim()) continue;
        const evt = JSON.parse(line);
        if (evt.type === 'start') {
          $("total").textContent = String(evt.total || 0);
        } else if (evt.type === 'result') {
          const r = evt.data;
          $("done").textContent = String(evt.done || 0);
          $("ok").textContent = String(evt.ok || 0);
          $("fail").textContent = String(evt.failed || 0);
          if (r.ok && r.keyRaw) okRawKeys.push(r.keyRaw);
          const tr = document.createElement('tr');
          tr.innerHTML = '<td>' + r.index + '</td>' +
            '<td>' + esc(r.keyMasked) + '</td>' +
            '<td>' + (r.statusCode||0) + '</td>' +
            '<td>' + (r.latencyMs||0) + '</td>' +
            '<td class="' + (r.ok ? 'ok' : 'bad') + '">' + (r.ok ? 'OK' : 'FAIL') + '</td>' +
            '<td class="muted">' + esc((r.error||'').slice(0,120)) + '</td>';
          tbody.prepend(tr);
        } else if (evt.type === 'summary') {
          setStatus(stopped ? 'stopped' : 'done');
        }
      }
    }
  } catch (e) {
    if (!stopped) {
      setStatus('error');
      alert('异常: ' + e.message);
    }
  } finally {
    running = false;
    activeReader = null;
    if (!stopped && $("status").textContent === 'running') setStatus('done');
  }
};
</script>
</body>
</html>`
