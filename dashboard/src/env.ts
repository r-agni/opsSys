/** Typed accessors for Vite env variables. All have sensible local dev defaults. */
export const env = {
  keycloakUrl:      import.meta.env.VITE_KEYCLOAK_URL      ?? 'https://keycloak.internal',
  keycloakRealm:    import.meta.env.VITE_KEYCLOAK_REALM    ?? 'systemscale',
  keycloakClientId: import.meta.env.VITE_KEYCLOAK_CLIENT_ID ?? 'dashboard',
  fleetApiUrl:      import.meta.env.VITE_FLEET_API_URL      ?? 'http://localhost:8080',
  queryApiUrl:      import.meta.env.VITE_QUERY_API_URL      ?? 'http://localhost:8081',
  commandApiUrl:    import.meta.env.VITE_COMMAND_API_URL    ?? 'http://localhost:8082',
  apikeyUrl:        import.meta.env.VITE_APIKEY_URL         ?? 'http://localhost:8083',
  wsGatewayUrl:     import.meta.env.VITE_WS_GATEWAY_URL    ?? 'ws://localhost:8080',
} as const
