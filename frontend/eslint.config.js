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
      ecmaVersion: 2020,
      globals: globals.browser,
    },
    rules: {
      // MUI 9 resolves `color` as a styled variant rather than a system prop,
      // so a dotted palette path silently emits no CSS. See issue #65.
      'no-restricted-syntax': [
        'error',
        {
          selector: 'JSXAttribute[name.name="color"] Literal[value=/[a-zA-Z]\\.|\\.[a-zA-Z]/]',
          message:
            'A dotted palette path in the `color` prop emits no CSS in MUI 9. Use the variant name (color="textSecondary", color="error") or sx={{ color: "text.secondary" }}.',
        },
        {
          selector: 'JSXAttribute[name.name="color"] TemplateElement[value.raw=/\\./]',
          message:
            'A dotted palette path in the `color` prop emits no CSS in MUI 9. Build the variant name instead (color={status ?? "textSecondary"}).',
        },
      ],
    },
  },
])
