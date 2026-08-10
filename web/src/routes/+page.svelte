<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Health } from '$lib/api';

  let health = $state<Health | null>(null);
  let failed = $state(false);

  onMount(async () => {
    try {
      health = await api.health();
    } catch {
      failed = true;
    }
  });
</script>

<section>
  <h1>dokkup</h1>

  {#if failed}
    <p class="bad">Could not reach the dokkup API.</p>
  {:else if health === null}
    <p class="muted">Checking…</p>
  {:else if health.status === 'ok'}
    <p class="good">Connected to Dokku {health.dokku}</p>
  {:else}
    <!--
      dokkup runs on the Dokku host but is not part of Dokku, so it can be up
      while Dokku is not. Saying which of the two is broken is the whole value of
      this screen.
    -->
    <p class="bad">dokkup is running, but cannot reach Dokku on this host.</p>
  {/if}

  <p class="muted">
    This is the project scaffold. The interface is not built yet — see the
    <a href="https://github.com/eduardotorresdev/dokkup">repository</a> for what is
    planned and why.
  </p>
</section>

<style>
  section {
    max-width: 42rem;
  }

  h1 {
    font-size: 1.5rem;
    letter-spacing: -0.02em;
    margin: 0 0 1rem;
  }

  p {
    margin: 0 0 0.75rem;
    line-height: 1.6;
  }

  .muted {
    color: var(--muted);
  }
  .good {
    color: var(--ok);
  }
  .bad {
    color: var(--bad);
  }
</style>
