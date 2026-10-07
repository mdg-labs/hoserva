// Text as the page renderer writes it into HTML: React escapes these five
// characters in text, so a catalog title shows up in a built page in this form.
export function escapeText(text) {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#x27;');
}
