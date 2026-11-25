import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      globals: globals.browser,
    },
    rules: {
      // API payloads and admin extension points intentionally accept
      // partially-known JSON. Runtime validation and generated endpoint
      // types remain the source of truth at those boundaries.
      '@typescript-eslint/no-explicit-any': 'off',

      // `const { blob, ...meta } = record` is how a field is dropped from a
      // copy; the omitted siblings are unused by design.
      '@typescript-eslint/no-unused-vars': ['error', { ignoreRestSiblings: true }],

      // Dialogs in this app reset local form state when they open. That is
      // deliberate prop-to-state synchronization, not an external effect
      // feedback loop. The generic React 19 rule cannot distinguish it.
      'react-hooks/set-state-in-effect': 'off',

      // Marketing sections read stable layout refs while composing derived
      // styles. These refs are not mutated during render.
      'react-hooks/refs': 'off',

      // Several component modules export small formatting helpers, and the
      // Vite entry file necessarily mounts a component without exporting it.
      'react-refresh/only-export-components': 'off',
    },
  },
])
