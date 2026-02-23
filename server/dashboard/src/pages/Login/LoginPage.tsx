import { useAuthStore } from '../../store/authStore'
import styles from './LoginPage.module.css'

export function LoginPage() {
  const login = useAuthStore((s) => s.login)
  const signup = useAuthStore((s) => s.signup)
  return (
    <div className={styles.root}>
      {/* Subtle radial gradient backdrop */}
      <div className={styles.glow} aria-hidden />

      <div className={styles.card}>
        {/* Logo mark */}
        <div className={styles.logoWrap}>
          <svg width="44" height="44" viewBox="0 0 44 44" fill="none" aria-hidden>
            <rect x="2" y="2" width="18" height="18" rx="4" fill="var(--color-accent)"/>
            <rect x="24" y="2" width="18" height="18" rx="4" fill="var(--color-accent)" opacity="0.55"/>
            <rect x="2" y="24" width="18" height="18" rx="4" fill="var(--color-accent)" opacity="0.55"/>
            <rect x="24" y="24" width="18" height="18" rx="4" fill="var(--color-accent)" opacity="0.2"/>
          </svg>
        </div>

        <h1 className={styles.brand}>SystemScale</h1>
        <p className={styles.sub}>Operator Dashboard</p>

        <button className={`primary ${styles.loginBtn}`} onClick={login}>
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden>
            <circle cx="8" cy="8" r="7" stroke="white" strokeWidth="1.3" opacity=".6"/>
            <path d="M8 4.5v7M4.5 8h7" stroke="white" strokeWidth="1.4" strokeLinecap="round"/>
          </svg>
          Sign in with Email
        </button>

        <div className={styles.divider}>
          <span>or</span>
        </div>

        <button className={styles.signupBtn} onClick={signup}>
          Create an account
        </button>

        <p className={styles.hint}>Secured by Keycloak OIDC · PKCE flow</p>
      </div>
    </div>
  )
}
