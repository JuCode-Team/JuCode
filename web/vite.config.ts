import { readFileSync } from 'node:fs'
import { defineConfig, loadEnv, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { resolveDeployment, type Deployment } from './src/config/deployment'

// Bundle layout:
//   - Tiny "main" entry with React/router shell + marketing pages.
//   - Heavy vendors are split into their own long-cached chunks so a
//     code change in app code doesn't bust the recharts/radix download.
//   - Console pages load as one async chunk (see config/consolePages.tsx),
//     so an anonymous visitor never pays for the dashboard JS, but tab
//     switches inside the console don't wait on further imports.
//
// Order matters: vendors are matched in order, first hit wins.
function vendorChunkFor(id: string): string | undefined {
  // recharts and @lobehub/icons are deliberately NOT split into their
  // own vendor chunk: doing so makes Vite preload them from index.html
  // (via <link rel="modulepreload">) even though only lazy-loaded
  // pages actually use them. Letting the bundler co-locate them with
  // the lazy route chunks keeps them off the marketing-page critical
  // path entirely.
  if (id.includes('node_modules/@radix-ui/themes')) return 'radix-themes';
  if (id.includes('node_modules/react-virtuoso')) return 'react-virtuoso';
  if (id.includes('node_modules/lucide-react')) return 'lucide';
  if (id.includes('node_modules/@tanstack/react-query')) return 'tanstack';
  if (id.includes('node_modules/react-router')) return 'react-router';
  // React core last so it doesn't gobble dom-utility packages above.
  if (id.includes('node_modules/react/') || id.includes('node_modules/react-dom/') || id.includes('node_modules/scheduler')) return 'react';
  return undefined;
}

// seo/llms.txt names the public origin, and index.html the site name; fill
// them in from the deployment config (see README). robots.txt is served as is.
const SEO_FILES = ['llms.txt', 'robots.txt']

function fillDeployment(text: string, d: Deployment): string {
  return text.replaceAll('%PUBLIC_SITE_URL%', d.publicSiteUrl).replaceAll('%SITE_NAME%', d.siteName)
}

function deploymentFiles(d: Deployment): Plugin {
  const read = (name: string) => fillDeployment(readFileSync(new URL(`./seo/${name}`, import.meta.url), 'utf-8'), d)
  return {
    name: 'deployment-files',
    transformIndexHtml: (html) => fillDeployment(html, d),
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const name = SEO_FILES.find((f) => req.url === `/${f}`)
        if (!name) return next()
        res.setHeader('Content-Type', 'text/plain; charset=utf-8')
        res.end(read(name))
      })
    },
    generateBundle() {
      for (const name of SEO_FILES) this.emitFile({ type: 'asset', fileName: name, source: read(name) })
    },
  }
}

// https://vite.dev/config/
export default defineConfig(({ mode }) => ({
  plugins: [react(), tailwindcss(), deploymentFiles(resolveDeployment(loadEnv(mode, process.cwd(), 'VITE_')))],
  build: {
    chunkSizeWarningLimit: 1400,
    rollupOptions: {
      output: {
        manualChunks(id) {
          return vendorChunkFor(id);
        },
      },
    },
  },
}))
