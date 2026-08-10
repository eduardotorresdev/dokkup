<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Session } from '$lib/api';
  import '../app.css';

  let { children } = $props();

  let session = $state<Session | null>(null);

  onMount(async () => {
    try {
      session = await api.session();
    } catch {
      // The banner is an added warning, never a gate. A failure to read the
      // session must not stop the page rendering, or a broken install becomes
      // undiagnosable through the interface.
      session = null;
    }
  });
</script>

<div class="shell">
  {#if session?.ownerOnly}
    <!--
      Not dismissible. Reaching dokkup by IP address means no certificate
      authority vouches for this connection, and the way to remove the warning
      is to publish a domain, not to close it.
    -->
    <div class="warning" role="alert">
      <strong>Not recommended:</strong> dokkup is reachable by IP address, so no
      certificate authority can vouch for this connection. Only the owner may
      sign in. Run <code>dokkup publish &lt;domain&gt;</code> on the host to serve
      it at a domain with a certificate.
    </div>
  {/if}

  <header>
    <span class="mark">dokkup</span>
    <span class="tagline">Dokku, with fewer terminals</span>
  </header>

  <main>
    {@render children()}
  </main>
</div>

<style>
  .shell {
    min-height: 100vh;
    display: flex;
    flex-direction: column;
  }

  .warning {
    background: var(--warn-bg);
    color: var(--warn-fg);
    border-bottom: 1px solid var(--warn-border);
    padding: 0.75rem 1.25rem;
    font-size: 0.9rem;
    line-height: 1.5;
  }

  header {
    display: flex;
    align-items: baseline;
    gap: 0.75rem;
    padding: 1.25rem;
    border-bottom: 1px solid var(--border);
  }

  .mark {
    font-weight: 650;
    letter-spacing: -0.02em;
  }

  .tagline {
    color: var(--muted);
    font-size: 0.875rem;
  }

  main {
    flex: 1;
    padding: 1.25rem;
  }
</style>
