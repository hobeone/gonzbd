// Apply theme before first paint to prevent flash. Loaded as a same-origin
// static file (not an inline <script>) so it is covered by the
// "script-src 'self'" CSP directive without requiring a script hash or
// 'unsafe-inline'.
(function () {
	var t = localStorage.getItem('theme');
	if (t === 'dark' || (t !== 'light' && matchMedia('(prefers-color-scheme: dark)').matches)) {
		document.documentElement.classList.add('dark');
	}
})();
