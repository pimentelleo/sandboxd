import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { AccountLogin, CreateLocalAccount, EntraLogin, Login } from './AuthGate'

describe('EntraLogin', () => {
  it('offers the Microsoft sign-in redirect when configured', () => {
    const signIn = vi.fn()
    render(<EntraLogin available onSignIn={signIn} />)

    fireEvent.click(screen.getByTestId('entra-login-submit'))

    expect(signIn).toHaveBeenCalledOnce()
  })

  it('renders a safe denied state without provider details', () => {
    render(<EntraLogin available notice="denied" />)

    expect(screen.getByTestId('entra-login-notice').textContent).toContain('did not authorize')
    expect(screen.queryByText(/AADSTS/i)).toBeNull()
  })

  it('disables sign-in when production auth is unavailable', () => {
    render(<EntraLogin available={false} />)

    expect((screen.getByTestId('entra-login-submit') as HTMLButtonElement).disabled).toBe(true)
  })

  it('retains a local sign-in screen after logout', () => {
    render(<Login onDone={vi.fn()} notice="logged_out" />)

    expect(screen.getByTestId('login-notice').textContent).toContain('signed out')
    expect(screen.getByTestId('login-password')).toBeTruthy()
  })

  it('collects email and password for local account setup', () => {
    render(<CreateLocalAccount onDone={vi.fn()} />)

    expect(screen.getByTestId('account-setup-email')).toBeTruthy()
    expect(screen.getByTestId('account-setup-password')).toBeTruthy()
    expect(screen.getByTestId('account-setup-confirm')).toBeTruthy()
  })

  it('renders a local account sign-in notice after logout', () => {
    render(<AccountLogin onDone={vi.fn()} notice="logged_out" />)

    expect(screen.getByTestId('account-login-notice').textContent).toContain('signed out')
    expect(screen.getByTestId('account-login-email')).toBeTruthy()
  })
})
