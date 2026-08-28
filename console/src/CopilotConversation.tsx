import { useCallback, useEffect, useRef, useState } from 'react'
import {
  api, ContextTier, ConversationChild, ConversationChildChange, ConversationEvent,
  ConversationInteraction, ConversationMessage, ConversationMode, ConversationSnapshot,
  ConversationTurn, CopilotModel, Sandbox,
} from './api'
import { c, Card, Btn, H, Input, mono, Pill } from './design/kit'

const ACTIVE_TURN_STATUSES = new Set(['running', 'waiting_input', 'waiting_plan', 'cancelling'])
const ACTIVE_CHILD_STATUSES = new Set(['queued', 'preparing', 'running', 'cancelling'])
type CatalogState = 'loading' | 'ready' | 'empty' | 'unavailable' | 'not_connected'

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
function isString(value: unknown): value is string {
  return typeof value === 'string'
}
function isNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value)
}
function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every(isString)
}

export function parseConversationPrompt(prompt: string, selectedMode: ConversationMode): { prompt: string; mode: ConversationMode } {
  const trimmed = prompt.trim()
  for (const [prefix, mode] of [
    ['/plan', 'plan'],
    ['/interactive', 'interactive'],
    ['/autopilot', 'autopilot'],
  ] as const) {
    if (trimmed === prefix) return { prompt: '', mode }
    if (trimmed.startsWith(`${prefix} `)) return { prompt: trimmed.slice(prefix.length).trim(), mode }
  }
  return { prompt: trimmed, mode: selectedMode }
}

// Events intentionally update only fields that are fully represented in their
// payload. A partial event returns null so the caller reloads authoritative state.
export function applyConversationEvent(snapshot: ConversationSnapshot, event: ConversationEvent): ConversationSnapshot | null {
  if (!isNumber(event.id) || !isString(event.type) || !isRecord(event.payload)) return null
  if (event.id <= snapshot.event_cursor) return snapshot
  const next = { ...snapshot, event_cursor: event.id }
  const payload = event.payload
  const turnID = isString(payload.turn_id) ? payload.turn_id : event.turn_id

  if (event.type === 'message.created') {
    if (!isString(payload.id) || !isString(turnID) || !isNumber(payload.sequence) || !isString(payload.role) || !isString(payload.content) || !isString(payload.status)) return null
    if (next.messages.some((message) => message.id === payload.id)) return next
    return {
      ...next,
      messages: [...next.messages, {
        id: payload.id, turn_id: turnID, sequence: payload.sequence, role: payload.role,
        content: payload.content, status: payload.status, created_at: event.created_at,
      }].sort((a, b) => a.sequence - b.sequence),
    }
  }
  if (event.type === 'message.delta') {
    if (!isString(payload.id) || !isString(turnID) || !isString(payload.text)) return null
    const existing = next.messages.find((message) => message.id === payload.id)
    if (existing) {
      return { ...next, messages: next.messages.map((message) => message.id === payload.id ? { ...message, content: message.content + payload.text, status: 'streaming' } : message) }
    }
    const sequence = Math.max(0, ...next.messages.map((message) => message.sequence)) + 1
    return {
      ...next,
      messages: [...next.messages, {
        id: payload.id, turn_id: turnID, sequence, role: 'assistant', content: payload.text,
        status: 'streaming', created_at: event.created_at,
      }],
    }
  }
  if (event.type === 'turn.started' || event.type === 'turn.cancelling') {
    if (!isString(turnID) || !next.turns.some((turn) => turn.id === turnID)) return null
    const status = event.type === 'turn.started' ? 'running' : 'cancelling'
    return { ...next, turns: next.turns.map((turn) => turn.id === turnID ? { ...turn, status } : turn) }
  }
  if (event.type === 'turn.completed') {
    if (!isString(turnID) || !isString(payload.status) || !next.turns.some((turn) => turn.id === turnID)) return null
    return {
      ...next,
      turns: next.turns.map((turn) => turn.id === turnID ? {
        ...turn, status: payload.status, error_message: isString(payload.error_message) ? payload.error_message : undefined,
      } : turn),
    }
  }
  if (event.type === 'interaction.requested') {
    if (!isString(payload.id) || !isString(turnID) || !isNumber(payload.sequence) || !isString(payload.type) || !isString(payload.status) ||
      !Array.isArray(payload.choices) || !payload.choices.every(isString) || !Array.isArray(payload.actions) || !payload.actions.every(isString) ||
      typeof payload.allow_freeform !== 'boolean') return null
    if (next.interactions.some((interaction) => interaction.id === payload.id)) return next
    return {
      ...next,
      interactions: [...next.interactions, {
        id: payload.id, turn_id: turnID, sequence: payload.sequence, type: payload.type, status: payload.status,
        question: isString(payload.question) ? payload.question : undefined, choices: payload.choices,
        allow_freeform: payload.allow_freeform, summary: isString(payload.summary) ? payload.summary : undefined,
        plan: isString(payload.plan) ? payload.plan : undefined, actions: payload.actions,
        recommended_action: isString(payload.recommended_action) ? payload.recommended_action : undefined,
        created_at: event.created_at,
      }].sort((a, b) => a.sequence - b.sequence),
    }
  }
  if (event.type === 'interaction.resolved') {
    if (!isString(payload.id) || !isString(payload.status) || !next.interactions.some((interaction) => interaction.id === payload.id)) return null
    return { ...next, interactions: next.interactions.map((interaction) => interaction.id === payload.id ? { ...interaction, status: payload.status } : interaction) }
  }
  if (event.type === 'child.created' || event.type === 'child.updated') {
    if (!isString(payload.id)) return null
    const existing = (next.children || []).find((child) => child.id === payload.id)
    const child = conversationChildFromEvent(payload, event, existing)
    if (!child) return null
    const children = existing
      ? (next.children || []).map((candidate) => candidate.id === child.id ? child : candidate)
      : [...(next.children || []), child]
    return { ...next, children }
  }
  if (event.type === 'tool' || event.type === 'usage') return next
  return null
}

function conversationChildFromEvent(payload: Record<string, unknown>, event: ConversationEvent, existing?: ConversationChild): ConversationChild | null {
  const task = isString(payload.task) ? payload.task : payload.prompt
  if (!isString(payload.id) || !isString(payload.parent_turn_id) || !isString(task) ||
    !isString(payload.model) || !isString(payload.reasoning_effort) || !isString(payload.context_tier) ||
    !isString(payload.status) || !isString(payload.result) || !isString(payload.patch_state) ||
    !isStringArray(payload.changed_files)) return null
  return {
    id: payload.id, parent_turn_id: payload.parent_turn_id, task,
    ...(isString(payload.label) ? { label: payload.label } : {}),
    model: payload.model, reasoning_effort: payload.reasoning_effort,
    context_tier: payload.context_tier as ContextTier, status: payload.status,
    result: payload.result, patch_state: payload.patch_state, changed_files: payload.changed_files,
    ...(isString(payload.error_message) ? { error_message: payload.error_message } : {}),
    created_at: existing?.created_at || event.created_at,
    ...(existing?.started_at ? { started_at: existing.started_at } : {}),
    ...(existing?.finished_at ? { finished_at: existing.finished_at } : {}),
  }
}

function conversationBubble(message: ConversationMessage) {
  const user = message.role === 'user'
  return (
    <div key={message.id} style={{ display: 'flex', justifyContent: user ? 'flex-end' : 'flex-start' }}>
      <div style={{ maxWidth: '85%', fontSize: 12.5, borderRadius: 9, padding: '8px 11px', whiteSpace: 'pre-wrap', background: user ? c.ink : c.panel2, color: user ? '#fff' : c.fg, border: user ? 'none' : `1px solid ${c.border}` }}>
        {message.content || (message.status === 'streaming' ? '…' : '')}
      </div>
    </div>
  )
}

function contextLimitHint(model: CopilotModel | undefined) {
  if (!model?.max_context_window_tokens) {
    return model ? 'Context limit not reported by Copilot.' : 'Choose a model to enable long context.'
  }
  return `Up to ${Math.round(model.max_context_window_tokens / 1000).toLocaleString()}k tokens of context.`
}

function InputInteraction({ interaction, sandboxId, onDone, onError }: { interaction: ConversationInteraction; sandboxId: string; onDone: () => void; onError: (message: string) => void }) {
  const [answer, setAnswer] = useState('')
  const [sending, setSending] = useState(false)
  const submit = async (value: string) => {
    if (!value.trim() || sending) return
    setSending(true)
    try { await api.answerConversationInteraction(sandboxId, interaction.id, value.trim()); onDone() }
    catch (error) { onError((error as Error).message) } finally { setSending(false) }
  }
  return (
    <Card style={{ padding: 12, background: c.panel3 }} data-testid={`copilot-interaction-${interaction.id}`}>
      <H size={13}>Copilot needs input</H>
      <div style={{ marginTop: 5, fontSize: 12.5, whiteSpace: 'pre-wrap' }}>{interaction.question || 'Choose a response.'}</div>
      {interaction.choices.length > 0 && <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 10 }}>
        {interaction.choices.map((choice, index) => <Btn key={choice} sm disabled={sending} onClick={() => submit(choice)} data-testid={`copilot-choice-${interaction.id}-${index}`}>{choice}</Btn>)}
      </div>}
      {interaction.allow_freeform && <div style={{ display: 'flex', gap: 6, marginTop: 9 }}>
        <Input value={answer} onChange={(event) => setAnswer(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); submit(answer) } }} placeholder="Your response…" aria-label="Response to Copilot" style={{ flex: 1 }} data-testid={`copilot-freeform-${interaction.id}`} />
        <Btn sm variant="primary" disabled={sending || !answer.trim()} onClick={() => submit(answer)} data-testid={`copilot-answer-${interaction.id}`}>Send</Btn>
      </div>}
    </Card>
  )
}

function PlanInteraction({ interaction, sandboxId, onDone, onError }: { interaction: ConversationInteraction; sandboxId: string; onDone: () => void; onError: (message: string) => void }) {
  const [feedback, setFeedback] = useState('')
  const [action, setAction] = useState(interaction.recommended_action || interaction.actions[0] || '')
  const [sending, setSending] = useState(false)
  const respond = async (approved: boolean) => {
    if (sending || (approved && !interaction.actions.includes(action))) return
    setSending(true)
    try {
      await api.answerConversationPlan(sandboxId, interaction.id, {
        approved, ...(approved ? { selected_action: action } : {}), ...(feedback.trim() ? { feedback: feedback.trim() } : {}),
      })
      onDone()
    } catch (error) { onError((error as Error).message) } finally { setSending(false) }
  }
  return (
    <Card style={{ padding: 12, background: c.panel3 }} data-testid={`copilot-plan-${interaction.id}`}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}><H size={13}>Review Copilot’s plan</H><Pill tone="warn">approval needed</Pill></div>
      {interaction.summary && <div style={{ marginTop: 6, fontSize: 12.5 }}>{interaction.summary}</div>}
      {interaction.plan && <pre style={{ margin: '9px 0', padding: '9px 10px', border: `1px solid ${c.border}`, borderRadius: 7, background: c.bg, whiteSpace: 'pre-wrap', ...mono, fontSize: 11.5 }}>{interaction.plan}</pre>}
      <label style={{ display: 'block', fontSize: 11.5, color: c.muted, marginBottom: 4 }} htmlFor={`copilot-feedback-${interaction.id}`}>Feedback (optional)</label>
      <textarea id={`copilot-feedback-${interaction.id}`} value={feedback} onChange={(event) => setFeedback(event.target.value)} rows={2} data-testid={`copilot-feedback-${interaction.id}`} style={{ width: '100%', resize: 'vertical', background: c.panel, border: `1px solid ${c.border2}`, borderRadius: 7, padding: '7px 9px', color: c.fg, fontFamily: 'inherit', fontSize: 12.5 }} />
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 9, alignItems: 'center' }}>
        {interaction.actions.map((candidate) => <button key={candidate} type="button" onClick={() => setAction(candidate)} disabled={sending} aria-pressed={action === candidate} data-testid={`copilot-plan-action-${interaction.id}-${candidate}`} style={{ border: `1px solid ${action === candidate ? c.ink : c.border2}`, borderRadius: 6, padding: '5px 8px', background: action === candidate ? c.panel2 : c.panel, color: c.fg, fontSize: 11.5 }}>{candidate}</button>)}
        <span style={{ flex: 1 }} />
        <Btn sm variant="danger" disabled={sending} onClick={() => respond(false)} data-testid={`copilot-deny-${interaction.id}`}>Deny{feedback.trim() ? ' + feedback' : ''}</Btn>
        <Btn sm variant="primary" disabled={sending || !interaction.actions.includes(action)} onClick={() => respond(true)} data-testid={`copilot-approve-${interaction.id}`}>Approve</Btn>
      </div>
    </Card>
  )
}

function childStatusTone(status: string): 'good' | 'warn' | 'bad' | 'neutral' {
  if (status === 'succeeded') return 'good'
  if (status === 'failed' || status === 'interrupted') return 'bad'
  if (ACTIVE_CHILD_STATUSES.has(status)) return 'warn'
  return 'neutral'
}

function DelegatedTask({ child, sandboxId, onDone, onError }: {
  child: ConversationChild
  sandboxId: string
  onDone: () => void
  onError: (message: string) => void
}) {
  const [change, setChange] = useState<ConversationChildChange | null>(null)
  const [reviewingPath, setReviewingPath] = useState('')
  const [cancelling, setCancelling] = useState(false)
  const active = ACTIVE_CHILD_STATUSES.has(child.status)
  const review = async (path: string) => {
    if (reviewingPath) return
    setReviewingPath(path)
    try {
      setChange(await api.getConversationChildChange(sandboxId, child.id, path))
    } catch (error) {
      onError((error as Error).message)
    } finally {
      setReviewingPath('')
    }
  }
  const cancel = async () => {
    if (cancelling) return
    setCancelling(true)
    try {
      await api.cancelConversationChild(sandboxId, child.id)
      onDone()
    } catch (error) {
      onError((error as Error).message)
    } finally {
      setCancelling(false)
    }
  }
  return (
    <article data-testid={`copilot-child-${child.id}`} style={{ border: `1px solid ${c.border}`, borderRadius: 8, background: c.panel3, padding: 11 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <H size={13}>{child.label || 'Delegated task'}</H>
        <Pill tone={childStatusTone(child.status)} dot={active}>{child.status.replace('_', ' ')}</Pill>
        {active && <Btn sm variant="danger" disabled={cancelling} onClick={cancel} data-testid={`copilot-child-cancel-${child.id}`}>Cancel</Btn>}
      </div>
      <div style={{ marginTop: 6, color: c.fg2, fontSize: 12.5, whiteSpace: 'pre-wrap' }}>{child.task}</div>
      <div style={{ marginTop: 7, color: c.muted, fontSize: 11.5 }}>
        {child.model || 'Copilot default'} · {child.reasoning_effort || 'default effort'} · {child.context_tier.replace('_', ' ')}
      </div>
      <div role="note" style={{ marginTop: 8, color: c.muted2, fontSize: 11.5 }}>
        Changes stay isolated until you review and apply them manually.
      </div>
      {child.result && <pre style={{ margin: '9px 0 0', maxHeight: 144, overflow: 'auto', padding: '8px 9px', border: `1px solid ${c.border}`, borderRadius: 6, background: c.bg, color: c.fg2, whiteSpace: 'pre-wrap', ...mono, fontSize: 11.25 }}>{child.result}</pre>}
      {child.error_message && <div style={{ marginTop: 8, color: c.bad, fontSize: 12 }}>{child.error_message}</div>}
      {child.changed_files.length > 0 && <div style={{ marginTop: 9 }}>
        <div style={{ color: c.muted, fontSize: 11.5, marginBottom: 5 }}>Review changed files</div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 5 }}>
          {child.changed_files.map((path) => <Btn key={path} sm variant="ghost" disabled={Boolean(reviewingPath)} onClick={() => review(path)} data-testid={`copilot-child-review-${child.id}-${path}`}>
            {reviewingPath === path ? 'Loading…' : path}
          </Btn>)}
        </div>
      </div>}
      {change && <section data-testid={`copilot-child-change-${child.id}`} style={{ marginTop: 10, borderTop: `1px solid ${c.border}`, paddingTop: 9 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}><H size={12}>Review: {change.path}</H><Pill tone={change.deleted ? 'bad' : 'neutral'}>{change.deleted ? 'deleted' : 'replacement'}</Pill></div>
        {change.base_sha256 && <div style={{ marginTop: 5, color: c.muted2, fontSize: 10.5, ...mono }}>Base {change.base_sha256}</div>}
        <pre style={{ margin: '7px 0 0', maxHeight: 210, overflow: 'auto', padding: '8px 9px', border: `1px solid ${c.border}`, borderRadius: 6, background: c.bg, color: c.fg, whiteSpace: 'pre-wrap', ...mono, fontSize: 11.25 }}>
          {change.deleted ? '(file deleted)' : change.content || '(empty file)'}
        </pre>
      </section>}
    </article>
  )
}

export function CopilotConversation({ sb, onError, toast, refresh }: { sb: Sandbox | null; onError: (message: string) => void; toast: (message: string) => void; refresh: () => void }) {
  const [snapshot, setSnapshot] = useState<ConversationSnapshot | null>(null)
  const [text, setText] = useState('')
  const [mode, setMode] = useState<ConversationMode>('interactive')
  const [models, setModels] = useState<CopilotModel[]>([])
  const [catalogState, setCatalogState] = useState<CatalogState>('loading')
  const [model, setModel] = useState('')
  const [reasoningEffort, setReasoningEffort] = useState('')
  const [contextTier, setContextTier] = useState<ContextTier>('default')
  const [loading, setLoading] = useState(false)
  const [sending, setSending] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const sourceRef = useRef<EventSource | null>(null)
  const reconnectRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const snapshotRef = useRef<ConversationSnapshot | null>(null)
  const cursorRef = useRef(0)
  const sandboxId = sb?.id

  const loadModels = useCallback(async () => {
    setCatalogState('loading')
    try {
      const response = await api.getGitHubCopilotModels()
      setModels(response.models)
      setCatalogState(response.models.length > 0 ? 'ready' : 'empty')
    } catch (error) {
      setModels([])
      setCatalogState((error as { status?: number }).status === 409 ? 'not_connected' : 'unavailable')
    }
  }, [])

  const loadSnapshot = useCallback(async () => {
    if (!sandboxId) { snapshotRef.current = null; setSnapshot(null); return }
    if (snapshotRef.current?.conversation?.sandbox_id !== sandboxId) {
      snapshotRef.current = null
      setSnapshot(null)
      cursorRef.current = 0
    }
    setLoading(true)
    try {
      const next = await api.getConversation(sandboxId)
      cursorRef.current = next.event_cursor
      snapshotRef.current = next
      setSnapshot(next)
      if (next.conversation?.default_mode) setMode(next.conversation.default_mode)
    } catch (error) { onError((error as Error).message) } finally { setLoading(false) }
  }, [sandboxId, onError])

  useEffect(() => { loadSnapshot() }, [loadSnapshot])
  useEffect(() => { void loadModels() }, [loadModels])
  useEffect(() => { if (scrollRef.current) scrollRef.current.scrollTop = scrollRef.current.scrollHeight }, [snapshot])

  useEffect(() => {
    if (!sandboxId || !snapshot?.conversation || snapshotRef.current !== snapshot) return
    let disposed = false
    const connect = () => {
      if (disposed) return
      const source = new EventSource(api.conversationEventsURL(sandboxId, cursorRef.current))
      sourceRef.current = source
      source.onmessage = (message) => {
        let event: ConversationEvent
        try { event = JSON.parse(message.data) as ConversationEvent } catch { loadSnapshot(); return }
        const current = snapshotRef.current
        if (!current) { loadSnapshot(); return }
        const next = applyConversationEvent(current, event)
        if (next === null) { loadSnapshot(); return }
        cursorRef.current = next.event_cursor
        snapshotRef.current = next
        setSnapshot(next)
      }
      source.onerror = () => {
        source.close()
        if (!disposed) reconnectRef.current = setTimeout(() => { loadSnapshot().finally(connect) }, 1000)
      }
    }
    connect()
    return () => {
      disposed = true
      sourceRef.current?.close()
      if (reconnectRef.current) clearTimeout(reconnectRef.current)
    }
  }, [sandboxId, snapshot?.conversation?.id]) // reconnects use the latest cursor ref, not state churn

  const turns = snapshot?.turns || []
  const active = turns.some((turn) => ACTIVE_TURN_STATUSES.has(turn.status))
  const queued = turns.filter((turn) => turn.status === 'queued')
  const pending = (snapshot?.interactions || []).filter((interaction) => interaction.status === 'pending')
  const children = snapshot?.children || []
  const resetAllowed = !active && queued.length === 0
  const selectedModel = models.find((candidate) => candidate.id === model)
  const reasoningEfforts = selectedModel?.supported_reasoning_efforts || []
  const catalogReady = catalogState === 'ready'
  const setSelectedModel = (nextModel: string) => {
    setModel(nextModel)
    const next = models.find((candidate) => candidate.id === nextModel)
    setReasoningEffort((current) => next?.supported_reasoning_efforts.includes(current) ? current : '')
    if (!nextModel) setContextTier('default')
  }
  const submit = async () => {
    if (!sandboxId || sending) return
    const parsed = parseConversationPrompt(text, mode)
    if (!parsed.prompt) { if (text.trim().startsWith('/')) setMode(parsed.mode); return }
    setSending(true)
    try {
      await api.sendConversationMessage(sandboxId, parsed.prompt, parsed.mode, {
        model, reasoning_effort: reasoningEffort, context_tier: contextTier,
      })
      setText('')
      setMode(parsed.mode)
      await loadSnapshot()
    } catch (error) { onError((error as Error).message) } finally { setSending(false) }
  }
  const cancel = async () => {
    if (!sandboxId) return
    try { await api.cancelConversation(sandboxId); toast('Cancelling Copilot…'); await loadSnapshot() }
    catch (error) { onError((error as Error).message) }
  }
  const reset = async () => {
    if (!sandboxId || !resetAllowed) return
    try { await api.resetConversation(sandboxId, mode); toast('Started a new Copilot conversation'); await loadSnapshot(); refresh() }
    catch (error) { onError((error as Error).message) }
  }

  return (
    <div data-testid="copilot-conversation" style={{ height: 640 }}>
    <Card style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '10px 14px', borderBottom: `1px solid ${c.border}`, background: c.panel3 }}>
        <H size={14}>GitHub Copilot</H>
        {active && <Pill tone="warn" dot>working</Pill>}
        {queued.length > 0 && <Pill tone="neutral">{queued.length} queued</Pill>}
        <Btn sm variant="ghost" disabled={!resetAllowed || !sandboxId} title={resetAllowed ? 'Archive this transcript and begin a new one' : 'Wait until active and queued work finishes'} onClick={reset} data-testid="copilot-reset" style={{ marginLeft: 'auto' }}>New conversation</Btn>
        {active && <Btn sm variant="danger" onClick={cancel} data-testid="copilot-cancel">Cancel</Btn>}
      </div>
      <div ref={scrollRef} style={{ flex: 1, overflowY: 'auto', padding: 14, display: 'flex', flexDirection: 'column', gap: 10 }}>
        {loading && !snapshot && <div style={{ color: c.muted2, fontSize: 12.5 }}>Loading conversation…</div>}
        {!loading && snapshot && snapshot.messages.length === 0 && !active && queued.length === 0 && <div style={{ margin: 'auto', maxWidth: 260, textAlign: 'center', color: c.muted2, fontSize: 12.5 }}>Start a durable Copilot conversation. Its transcript stays with this sandbox.</div>}
        {[...(snapshot?.messages || [])].sort((a, b) => a.sequence - b.sequence).map(conversationBubble)}
        {turns.filter((turn) => ACTIVE_TURN_STATUSES.has(turn.status) || turn.status === 'queued').map((turn) => <div key={turn.id} style={{ ...mono, fontSize: 11, color: turn.status === 'queued' ? c.muted2 : c.warn }}>▸ {turn.status === 'queued' ? `Queued: ${turn.prompt}` : `${turn.status.replace('_', ' ')}…`}</div>)}
        {pending.map((interaction) => interaction.type === 'plan'
          ? <PlanInteraction key={interaction.id} interaction={interaction} sandboxId={sandboxId!} onDone={loadSnapshot} onError={onError} />
          : <InputInteraction key={interaction.id} interaction={interaction} sandboxId={sandboxId!} onDone={loadSnapshot} onError={onError} />)}
        {children.length > 0 && <section aria-label="Delegated tasks" style={{ display: 'flex', flexDirection: 'column', gap: 7, marginTop: 2 }}>
          <H size={13}>Delegated tasks</H>
          {children.map((child) => <DelegatedTask key={child.id} child={child} sandboxId={sandboxId!} onDone={loadSnapshot} onError={onError} />)}
        </section>}
        {snapshot?.conversation?.last_error && <div style={{ color: c.bad, fontSize: 12.5 }}>{snapshot.conversation.last_error}</div>}
      </div>
      <div style={{ borderTop: `1px solid ${c.border}`, background: c.panel3 }}>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-end', gap: 8, padding: '8px 10px', borderBottom: `1px solid ${c.border}` }}>
          <label style={composerLabelStyle}>Model
            <select value={model} onChange={(event) => setSelectedModel(event.target.value)} disabled={!catalogReady || sending} aria-label="Copilot model" data-testid="copilot-model" style={{ ...selectStyle, minWidth: 178 }}>
              <option value="">Sandboxd / Copilot default</option>
              {models.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.name}</option>)}
            </select>
          </label>
          <label style={composerLabelStyle}>Reasoning
            <select value={reasoningEffort} onChange={(event) => setReasoningEffort(event.target.value)} disabled={!catalogReady || !selectedModel || reasoningEfforts.length === 0 || sending} aria-label="Reasoning effort" data-testid="copilot-reasoning-effort" style={selectStyle}>
              <option value="">Model default</option>
              {reasoningEfforts.map((effort) => <option key={effort} value={effort}>{effort}</option>)}
            </select>
          </label>
          <label style={composerLabelStyle}>Context
            <select value={contextTier} onChange={(event) => setContextTier(event.target.value as ContextTier)} disabled={sending} aria-describedby="copilot-context-hint" aria-label="Context window" data-testid="copilot-context-tier" style={selectStyle}>
              <option value="default">Standard</option>
              <option value="long_context" disabled={!selectedModel}>Long context</option>
            </select>
          </label>
          <span id="copilot-context-hint" data-testid="copilot-context-hint" style={{ alignSelf: 'center', color: c.muted2, fontSize: 11.5 }}>{contextLimitHint(selectedModel)}</span>
          {catalogState === 'loading' && <span style={{ alignSelf: 'center', color: c.muted2, fontSize: 11.5 }}>Loading models…</span>}
          {catalogState === 'empty' && <span style={{ alignSelf: 'center', color: c.muted2, fontSize: 11.5 }}>No selectable models returned.</span>}
          {catalogState === 'not_connected' && <span role="status" data-testid="copilot-model-catalog-error" style={{ alignSelf: 'center', color: c.warn, fontSize: 11.5 }}>Connect GitHub Copilot to load models.</span>}
          {catalogState === 'unavailable' && <span role="status" data-testid="copilot-model-catalog-error" style={{ display: 'inline-flex', alignItems: 'center', gap: 5, alignSelf: 'center', color: c.warn, fontSize: 11.5 }}>Models are temporarily unavailable.<Btn sm variant="ghost" disabled={sending} onClick={loadModels} data-testid="copilot-model-retry">Retry</Btn></span>}
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'flex-end', padding: 10 }}>
          <select value={mode} onChange={(event) => setMode(event.target.value as ConversationMode)} aria-label="Conversation mode" data-testid="copilot-mode" style={selectStyle}>
            <option value="interactive">Interactive</option><option value="plan">Plan</option><option value="autopilot">Autopilot</option>
          </select>
          <textarea value={text} onChange={(event) => setText(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); submit() } }} placeholder={sandboxId ? 'Message Copilot…' : 'Create a sandbox to start a conversation'} aria-label="Message Copilot" data-testid="copilot-prompt" rows={1} disabled={!sandboxId || sending} style={{ flex: 1, resize: 'none', background: c.panel, border: `1px solid ${c.border2}`, borderRadius: 7, padding: '8px 11px', color: c.fg, fontFamily: 'inherit', fontSize: 12.5 }} />
          <Btn variant="primary" disabled={!sandboxId || sending || !text.trim()} onClick={submit} data-testid="copilot-send">Send</Btn>
        </div>
      </div>
    </Card>
    </div>
  )
}

const selectStyle: React.CSSProperties = { background: c.bg, border: `1px solid ${c.border2}`, borderRadius: 7, padding: '5px 7px', color: c.fg, fontSize: 11.5 }
const composerLabelStyle: React.CSSProperties = { display: 'grid', gap: 3, color: c.muted, fontSize: 10.5, fontWeight: 600 }
