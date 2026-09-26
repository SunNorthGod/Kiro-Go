import { useState } from 'react'
import { Eye, EyeOff, Plug } from 'lucide-react'
import { maskKey } from '../api'
import { Button } from '@/ui/Button'
import { Badge, Segmented } from '@/ui/controls'
import { CopyButton } from '@/ui/misc'
import { useIsMobile } from '@/lib/media'

type Client = 'curl' | 'claude-code' | 'openai'

export function UsageTab({ apiKey }: { apiKey: string }) {
  const [reveal, setReveal] = useState(false)
  const [client, setClient] = useState<Client>('curl')
  const base = location.origin
  const mobile = useIsMobile()
  const shown = reveal ? apiKey : maskKey(apiKey)

  const snippet = (k: string) => {
    if (client === 'claude-code') {
      return `export ANTHROPIC_BASE_URL=${base}\nexport ANTHROPIC_AUTH_TOKEN=${k}\nclaude`
    }
    if (client === 'openai') {
      return `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}/v1", api_key="${k}")\nresp = client.chat.completions.create(\n    model="claude-sonnet-4",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(resp.choices[0].message.content)`
    }
    return `curl ${base}/v1/messages \\\n  -H "Authorization: Bearer ${k}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'`
  }

  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="card">
        <div className="card-header">
          <span className="card-title">
            <Plug />
            接入信息
          </span>
        </div>
        <div className="card-body stack" style={{ gap: 16 }}>
          <p className="sm text-2">把客户端的接口地址改成下面的 Base URL，再把你的 API Key 填到令牌或 API Key 的位置。</p>
          <div className="grid grid-2" style={{ gap: 14 }}>
            <div className="field">
              <span className="field-label">Base URL</span>
              <div className="copy-line">
                <code>{base}</code>
                <CopyButton value={base} label="复制地址" done="地址已复制" />
              </div>
            </div>
            <div className="field">
              <span className="field-label">API Key</span>
              <div className="copy-line">
                <code>{shown}</code>
                <Button size="sm" variant="ghost" iconOnly icon={reveal ? <EyeOff /> : <Eye />} onClick={() => setReveal((r) => !r)}>
                  {reveal ? '隐藏' : '显示'}
                </Button>
                <CopyButton value={apiKey} label="复制 Key" done="Key 已复制" />
              </div>
            </div>
          </div>
          <div className="stack" style={{ gap: 8 }}>
            <span className="field-label">接口</span>
            {[
              { name: 'Claude Messages', path: '/v1/messages' },
              { name: 'OpenAI Chat Completions', path: '/v1/chat/completions' },
              { name: 'OpenAI Responses', path: '/v1/responses' },
              { name: '模型列表', path: '/v1/models', method: 'GET' },
            ].map((e) => (
              <div key={e.path} className="copy-line" style={{ paddingLeft: 8 }}>
                <Badge tone={e.method === 'GET' ? 'teal' : 'accent'}>{e.method || 'POST'}</Badge>
                <code>
                  {/* 手机上 Base URL 就在上面，这里只留路径；复制出去的仍是完整地址 */}
                  {!mobile && base}
                  <b style={{ fontWeight: 600 }}>{e.path}</b>
                </code>
                {!mobile && (
                  <span className="xs muted nowrap" style={{ marginRight: 4 }}>
                    {e.name}
                  </span>
                )}
                <CopyButton value={base + e.path} label="复制" done="已复制" />
              </div>
            ))}
          </div>
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <span className="card-title">示例</span>
          <div className="card-actions">
            <Segmented
              id="usage-client"
              size="sm"
              value={client}
              onChange={setClient}
              items={[
                { value: 'curl', label: 'cURL' },
                { value: 'claude-code', label: 'Claude Code' },
                { value: 'openai', label: 'OpenAI SDK' },
              ]}
            />
          </div>
        </div>
        <div className="card-body stack" style={{ gap: 10 }}>
          <div style={{ position: 'relative' }}>
            <pre className="code">{snippet(shown)}</pre>
            <div style={{ position: 'absolute', top: 6, right: 6 }}>
              <CopyButton value={() => snippet(apiKey)} label="复制示例" done="示例已复制（含完整 Key）" variant="secondary" />
            </div>
          </div>
          <p className="xs muted">示例里的 Key 默认打码，复制时会带上完整 Key。opencode、Cherry Studio 等客户端同样填 Base URL 和 Key 即可。</p>
        </div>
      </div>
    </div>
  )
}
