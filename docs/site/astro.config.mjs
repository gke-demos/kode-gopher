// @ts-check
import { defineConfig } from 'astro/config';
import { unified } from '@astrojs/markdown-remark';
import starlight from '@astrojs/starlight';
import starlightLlmsTxt from 'starlight-llms-txt';
import { remarkPrependBase } from './src/plugins/remark-prepend-base.mjs';

// The GitHub Pages path. Content links are written root-relative
// (`/deploy/`) and remark-prepend-base adds this at build time, so
// changing it here moves nothing in the content.
const BASE = '/kode-gopher';

// Mirrors the go-steer sites (core-agent, k8s-lookout): light-only
// theme, the same palette and layout fixes in src/styles/theme.css,
// and core-agent's Hero component.
export default defineConfig({
  site: 'https://gke-demos.github.io',
  base: BASE,
  markdown: {
    processor: unified({ remarkPlugins: [remarkPrependBase(BASE)] }),
  },
  integrations: [
    starlight({
      title: 'kode-gopher',
      description:
        'Code Mode for Google Cloud in Go: an MCP server that compiles and runs the Go your agent writes, in a sandboxed Kubernetes pod.',
      logo: undefined,
      favicon: '/favicon.svg',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/gke-demos/kode-gopher',
        },
      ],
      editLink: {
        baseUrl:
          'https://github.com/gke-demos/kode-gopher/edit/main/docs/site/',
      },
      // Pins data-theme to 'light' before Starlight's own script runs.
      head: [
        {
          tag: 'script',
          attrs: { 'is:inline': true },
          content: "document.documentElement.dataset.theme = 'light';",
        },
      ],
      plugins: [
        starlightLlmsTxt({
          projectName: 'kode-gopher',
          description:
            'An MCP server (and CLI) that compiles and runs Go programs written by an AI agent in a sandboxed Kubernetes pod, against the real Google Cloud Go SDKs and client-go, and returns a structured result. Runs locally over stdio, or as a shared in-cluster server with Google sign-in.',
          promote: ['getting-started/**', 'reference/tools'],
          demote: ['deploy/operations'],
        }),
      ],
      customCss: ['./src/styles/theme.css'],
      // Empty ThemeSelect drops the dark-mode toggle (light-only site).
      components: {
        ThemeSelect: './src/components/ThemeSelect.astro',
        ThemeProvider: './src/components/ThemeProvider.astro',
        Hero: './src/components/Hero.astro',
      },
      sidebar: [
        {
          label: 'Overview',
          items: [
            { label: 'Introduction', link: '/' },
            { label: 'How it works', link: '/how-it-works/' },
          ],
        },
        {
          label: 'Getting started',
          items: [{ autogenerate: { directory: 'getting-started' } }],
        },
        {
          label: 'Deploy for a team',
          items: [{ autogenerate: { directory: 'deploy' } }],
        },
        {
          label: 'Concepts',
          items: [{ autogenerate: { directory: 'concepts' } }],
        },
        {
          label: 'Reference',
          items: [{ autogenerate: { directory: 'reference' } }],
        },
      ],
    }),
  ],
});
