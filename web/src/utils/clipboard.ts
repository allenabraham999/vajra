// copyToClipboard writes text to the system clipboard and reports whether
// it succeeded.
//
// It prefers the async Clipboard API, but falls back to a hidden
// <textarea> + execCommand('copy'). The fallback is essential: the
// dashboard is served over plain HTTP, where navigator.clipboard is
// undefined because the Clipboard API only exists in a secure context.
// Calling navigator.clipboard.writeText directly on HTTP throws, which is
// why the copy buttons silently did nothing.
export async function copyToClipboard(text: string): Promise<boolean> {
  // Preferred path — available on HTTPS / localhost.
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Permission denied or transient failure — fall through to legacy.
    }
  }

  // Legacy fallback — works on plain HTTP and older browsers.
  try {
    const textarea = document.createElement('textarea')
    textarea.value = text
    textarea.style.position = 'fixed'
    textarea.style.left = '-999999px'
    textarea.style.top = '0'
    document.body.appendChild(textarea)
    textarea.focus()
    textarea.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(textarea)
    return ok
  } catch (e) {
    console.error('copy failed', e)
    return false
  }
}
