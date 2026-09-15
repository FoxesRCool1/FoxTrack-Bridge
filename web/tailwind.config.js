/** @type {import('tailwindcss').Config} */
// FoxTrack design tokens. Every colour below is a CSS variable defined in the
// <style> block of web/ui.html (Paper = light, Ink = dark), so utilities such
// as bg-panel, text-text-2, border-border and bg-brand follow the theme.
// Names mirror the FoxTrack web app (src/styles.css) so the two stay in step.
module.exports = {
  content: ['./web/ui.html'],
  theme: {
    extend: {
      colors: {
        bg: 'var(--bg)',
        panel: 'var(--panel)',
        'panel-2': 'var(--panel-2)',
        surface: 'var(--surface)',
        border: 'var(--border)',
        text: 'var(--text)',
        'text-2': 'var(--text-2)',
        brand: 'var(--accent)',
        'brand-2': 'var(--accent-2)',
        link: 'var(--link)',
        danger: 'var(--danger)',
        warning: 'var(--warning)',
      },
      // 12px for blocks, 8px for a row nested inside a block, 6px for small
      // controls, 4px for inline code and checkboxes. Pills are rounded-full.
      borderRadius: {
        xs: '4px',
        sm: '6px',
        md: '8px',
        lg: '12px',
        xl: '12px',
        '2xl': '16px',
      },
      fontFamily: {
        sans: ['Inter', 'ui-sans-serif', 'system-ui', '-apple-system', 'Segoe UI', 'Roboto', 'sans-serif'],
        heading: ['Cormorant Garamond', 'Times New Roman', 'Georgia', 'serif'],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'monospace'],
      },
    }
  }
}
