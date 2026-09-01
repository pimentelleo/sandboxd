import { useState } from 'react'
import { api } from './api'
import { c, font, Card, Btn, Input, H } from './design/kit'

// Full-page centered auth screens shown before the app when a session is required.
// Both are minimal single-card forms; Enter submits, errors render inline.

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100vh', background: c.bg, color: c.fg, fontFamily: font.sans, padding: 20 }}>
      <Card style={{ width: 360, maxWidth: '100%', padding: 28 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 9, marginBottom: 18 }}>
          <div style={{ width: 26, height: 26, borderRadius: 7, background: 'linear-gradient(135deg,#3f3f46,#18181b)', display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: font.mono, fontSize: 11, color: c.bg }}>&gt;_</div>
          <span style={{ fontFamily: font.display, fontWeight: 700, fontSize: 15, letterSpacing: '.2px' }}>sandboxd <span style={{ fontWeight: 500, color: c.muted }}>console</span></span>
        </div>
        {children}
      </Card>
    </div>
  )
}

export function Login({ onDone, notice }: {
  onDone: () => void
  notice?: 'expired' | 'logged_out'
}) {
  const [pw, setPw] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = () => {
    if (busy) return
    setErr('')
    setBusy(true)
    api.login(pw).then(onDone).catch((e) => { setErr((e as Error).message); setBusy(false) })
  }
  return (
    <Shell>
      <H size={18} style={{ marginBottom: 4 }}>Sign in</H>
      <div style={{ color: c.muted, fontSize: 12.5, marginBottom: 16 }}>Enter the console password to continue.</div>
      {notice === 'expired' && <div style={{ color: c.muted, fontSize: 12, marginBottom: 12 }} data-testid="login-notice">Your session expired. Sign in again to continue.</div>}
      {notice === 'logged_out' && <div style={{ color: c.muted, fontSize: 12, marginBottom: 12 }} data-testid="login-notice">You have signed out.</div>}
      <Input
        type="password"
        autoFocus
        value={pw}
        onChange={(e) => setPw(e.target.value)}
        onKeyDown={(e) => e.key === 'Enter' && submit()}
        placeholder="Password"
        style={{ width: '100%', boxSizing: 'border-box', marginBottom: 12 }}
        data-testid="login-password"
      />
      {err && <div style={{ color: c.bad, fontSize: 12, marginBottom: 12 }} data-testid="login-error">{err}</div>}
      <Btn variant="primary" disabled={busy} onClick={submit} style={{ width: '100%', padding: '9px 14px' }} data-testid="login-submit">Sign in</Btn>
    </Shell>
  )
}

type AccountNotice = 'expired' | 'logged_out'

function AccountForm({
  initial,
  notice,
  onDone,
}: {
  initial: boolean
  notice?: AccountNotice
  onDone: () => void
}) {
  const [email, setEmail] = useState('')
  const [pw, setPw] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (busy) return
    setErr('')
    if (initial && pw !== confirm) {
      setErr('Passwords do not match.')
      return
    }
    setBusy(true)
    const request = initial
      ? api.setupLocalAccount(email, pw)
      : api.loginLocalAccount(email, pw)
    request.then(onDone).catch((e) => {
      setErr((e as Error).message)
      setBusy(false)
    })
  }
  const testID = initial ? 'account-setup' : 'account-login'
  return (
    <Shell>
      <H size={18} style={{ marginBottom: 4 }}>{initial ? 'Set up sandboxd' : 'Sign in'}</H>
      <div style={{ color: c.muted, fontSize: 12.5, marginBottom: 16 }}>
        {initial
          ? 'Create the first administrator account for this local sandboxd instance.'
          : 'Use your sandboxd account to continue.'}
      </div>
      {notice === 'expired' && <div style={{ color: c.muted, fontSize: 12, marginBottom: 12 }} data-testid={`${testID}-notice`}>Your session expired. Sign in again to continue.</div>}
      {notice === 'logged_out' && <div style={{ color: c.muted, fontSize: 12, marginBottom: 12 }} data-testid={`${testID}-notice`}>You have signed out.</div>}
      <form onSubmit={submit}>
        <label htmlFor={`${testID}-email`} style={{ display: 'block', color: c.fg2, fontSize: 12, fontWeight: 500, marginBottom: 5 }}>Email address</label>
        <Input
          id={`${testID}-email`}
          type="email"
          autoComplete="email"
          autoFocus
          required
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
          style={{ width: '100%', boxSizing: 'border-box', marginBottom: 12 }}
          data-testid={`${testID}-email`}
        />
        <label htmlFor={`${testID}-password`} style={{ display: 'block', color: c.fg2, fontSize: 12, fontWeight: 500, marginBottom: 5 }}>Password</label>
        <Input
          id={`${testID}-password`}
          type="password"
          autoComplete={initial ? 'new-password' : 'current-password'}
          required
          minLength={8}
          value={pw}
          onChange={(e) => setPw(e.target.value)}
          placeholder="At least 8 characters"
          style={{ width: '100%', boxSizing: 'border-box', marginBottom: initial ? 10 : 12 }}
          data-testid={`${testID}-password`}
        />
        {initial && (
          <>
            <label htmlFor={`${testID}-confirm`} style={{ display: 'block', color: c.fg2, fontSize: 12, fontWeight: 500, marginBottom: 5 }}>Confirm password</label>
            <Input
              id={`${testID}-confirm`}
              type="password"
              autoComplete="new-password"
              required
              minLength={8}
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              placeholder="Repeat your password"
              style={{ width: '100%', boxSizing: 'border-box', marginBottom: 12 }}
              data-testid={`${testID}-confirm`}
            />
          </>
        )}
        {err && <div role="alert" style={{ color: c.bad, fontSize: 12, marginBottom: 12 }} data-testid={`${testID}-error`}>{err}</div>}
        <Btn type="submit" variant="primary" disabled={busy} style={{ width: '100%', padding: '9px 14px' }} data-testid={`${testID}-submit`}>
          {busy ? 'Please wait…' : initial ? 'Create administrator account' : 'Sign in'}
        </Btn>
      </form>
    </Shell>
  )
}

export function AccountLogin({ onDone, notice }: { onDone: () => void; notice?: AccountNotice }) {
  return <AccountForm initial={false} notice={notice} onDone={onDone} />
}

export function CreateLocalAccount({ onDone }: { onDone: () => void }) {
  return <AccountForm initial onDone={onDone} />
}

export function EntraLogin({ notice, available, onSignIn }: {
  notice?: 'denied' | 'expired' | 'logged_out' | 'unavailable'
  available: boolean
  onSignIn?: () => void
}) {
  const message = {
    denied: 'Your organization did not authorize this sign-in. Contact an administrator if you need access.',
    expired: 'Your session expired. Sign in again to continue.',
    logged_out: 'You have signed out.',
    unavailable: 'Enterprise sign-in is temporarily unavailable. Try again later.',
  }[notice || 'logged_out']
  const signIn = onSignIn || (() => { window.location.assign(api.entraLoginURL()) })
  return (
    <Shell>
      <H size={18} style={{ marginBottom: 4 }}>Sign in</H>
      <div style={{ color: c.muted, fontSize: 12.5, marginBottom: 16 }}>Use your organization account to access sandboxd.</div>
      {notice && <div style={{ color: notice === 'denied' || notice === 'unavailable' ? c.bad : c.muted, fontSize: 12, marginBottom: 12 }} data-testid="entra-login-notice">{message}</div>}
      {!available && !notice && <div style={{ color: c.bad, fontSize: 12, marginBottom: 12 }} data-testid="entra-login-unavailable">Enterprise sign-in is not configured.</div>}
      <Btn variant="primary" disabled={!available} onClick={signIn} style={{ width: '100%', padding: '9px 14px' }} data-testid="entra-login-submit">Sign in with Microsoft</Btn>
    </Shell>
  )
}

export function CreatePassword({ onDone }: { onDone: () => void }) {
  const [pw, setPw] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = () => {
    if (busy) return
    setErr('')
    if (pw.length < 8) { setErr('Password must be at least 8 characters.'); return }
    if (pw !== confirm) { setErr('Passwords do not match.'); return }
    setBusy(true)
    api.setupPassword(pw).then(onDone).catch((e) => { setErr((e as Error).message); setBusy(false) })
  }
  return (
    <Shell>
      <H size={18} style={{ marginBottom: 4 }}>Welcome to sandboxd</H>
      <div style={{ color: c.muted, fontSize: 12.5, marginBottom: 16 }}>Create a console password. You'll use it to sign in from now on.</div>
      <Input
        type="password"
        autoFocus
        value={pw}
        onChange={(e) => setPw(e.target.value)}
        onKeyDown={(e) => e.key === 'Enter' && submit()}
        placeholder="New password (min 8 characters)"
        style={{ width: '100%', boxSizing: 'border-box', marginBottom: 10 }}
        data-testid="setup-password"
      />
      <Input
        type="password"
        value={confirm}
        onChange={(e) => setConfirm(e.target.value)}
        onKeyDown={(e) => e.key === 'Enter' && submit()}
        placeholder="Confirm password"
        style={{ width: '100%', boxSizing: 'border-box', marginBottom: 12 }}
        data-testid="setup-confirm"
      />
      {err && <div style={{ color: c.bad, fontSize: 12, marginBottom: 12 }} data-testid="setup-error">{err}</div>}
      <Btn variant="primary" disabled={busy} onClick={submit} style={{ width: '100%', padding: '9px 14px' }} data-testid="setup-submit">Create password</Btn>
    </Shell>
  )
}
