# GoNZBD Frontend Design System

This document outlines the design system, styling guidelines, and UI tokens used by the GoNZBD Svelte 5 SPA, which is built on Svelte 5, Tailwind CSS v4 (using `@tailwindcss/vite`), and Shadcn Svelte.

---

## 1. Core Architecture & Tech Stack
*   **Framework**: Svelte 5 (Runes, Snippets, event handling)
*   **CSS Engine**: Tailwind CSS v4.0 (Vite plugin integration)
*   **Component Base**: Shadcn Svelte (using Bits UI primitives)
*   **Utilities**: `clsx` and `tailwind-merge` combined in a unified `cn()` helper

---

## 2. Design Tokens & Variables

The design system operates with custom CSS variables managed inside `@theme` in `ui/src/app.css`.

### Theme Color Palette

Below is the mapping of semantic theme colors configured in the CSS theme:

| CSS Variable | Default (Light Mode) | Dark Mode | Description |
| :--- | :--- | :--- | :--- |
| `--background` | `#ffffff` | `#09090b` | Base application background |
| `--foreground` | `#09090b` | `#fafafa` | Primary text color |
| `--card` | `#ffffff` | `#09090b` | Card container background |
| `--card-foreground`| `#09090b` | `#fafafa` | Card container text |
| `--popover` | `#ffffff` | `#09090b` | Dropdowns, dialogs, popovers |
| `--popover-foreground`| `#09090b` | `#fafafa` | Text inside popovers |
| `--primary` | `#2563eb` *(Blue 600)* | `#3b82f6` *(Blue 500)* | Primary brand, buttons, accents |
| `--primary-foreground`| `#f8fafc` | `#0f172a` | Text on top of primary colors |
| `--secondary` | `#f1f5f9` | `#27272a` | Secondary buttons/borders |
| `--secondary-foreground`| `#0f172a` | `#fafafa` | Secondary text |
| `--muted` | `#f1f5f9` | `#27272a` | De-emphasized areas |
| `--muted-foreground`| `#64748b` | `#a1a1aa` | De-emphasized labels |
| `--accent` | `#f1f5f9` | `#27272a` | Hover states |
| `--accent-foreground`| `#0f172a` | `#fafafa` | Hover state text |
| `--destructive` | `#ef4444` | `#7f1d1d` | Alert, delete, error status |
| `--destructive-foreground`| `#f8fafc` | `#fafafa` | Text on destructive elements |
| `--border` | `#e2e8f0` | `#27272a` | Component/layout borders |
| `--input` | `#e2e8f0` | `#27272a` | Form input borders |
| `--ring` | `#2563eb` | `#3b82f6` | Focus outlines |

### Border Radius
*   `--radius-lg` (`var(--radius)`): `0.5rem` (`8px`)
*   `--radius-md`: `calc(var(--radius) - 2px)` (`6px`)
*   `--radius-sm`: `calc(var(--radius) - 4px)` (`4px`)

### Animations
*   `--animate-accordion-down`: `accordion-down 0.2s ease-out`
*   `--animate-accordion-up`: `accordion-up 0.2s ease-out`

---

## 3. Custom Variants & Utilities

### Dark Mode Variant
Dark mode styles are scoped using the custom Tailwind v4 selector:
```css
@custom-variant dark (&:where(.dark, .dark *));
```

### Component Utility Classes
GoNZBD defines specific global utility classes inside `@layer components`:
*   `flex items-center gap-2 rounded-md bg-gray-800 px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-gray-700`

### Input Select Styling
To prevent browser default background mismatch in dark mode:
```css
select {
  color-scheme: light dark;
}
.dark select {
  background-color: #1f2937; /* gray-800 */
  color: #fafafa;
}
.dark select option {
  background-color: #1f2937;
  color: #fafafa;
}
```

---

## 4. Typography & Layout Guidelines

1.  **Typography**: Sans-serif defaults (system font stack) are styled via Tailwind's `font-sans` utilities. Text sizes are structured dynamically using semantic sizes (`text-xs`, `text-sm`, `text-md`, `text-lg`, `text-xl`).
2.  **Borders**: Interactive elements must inherit the generic `@apply border-border` to maintain layout line integrity in both themes.
3.  **Contrast**: Accessibility is prioritized by using high-contrast blue (`blue-600` in light, `blue-500` in dark) for primary call-to-actions, and clear statuses (green for success, amber/orange for warning, red for error).
