import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * dokkup's frontend is a fully client-rendered single-page application, built
 * into the Go binary rather than served by a Node process.
 *
 * SvelteKit is used here as a build tool, not as a server runtime. Rendering on
 * the server would mean a runtime dependency on the Dokku host, which is exactly
 * what installing a single file is meant to avoid.
 *
 * See docs/adr/0004-single-go-binary-with-embedded-csr-frontend.md.
 *
 * @type {import('@sveltejs/kit').Config}
 */
const config = {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({
      // Built straight into the Go module's embed directory: there is no
      // copy step to forget.
      pages: '../internal/server/static/dist',
      assets: '../internal/server/static/dist',
      // Every unknown path is a client-side route, so they all get the shell.
      fallback: 'index.html',
      precompress: false,
      strict: true
    })
  }
};

export default config;
