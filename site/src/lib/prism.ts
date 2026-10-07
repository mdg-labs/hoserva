import type * as Preset from '@docusaurus/preset-classic';

type PrismTheme = NonNullable<NonNullable<Preset.ThemeConfig['prism']>['theme']>;

// Token colours are CSS variables defined per colour mode in css/infima.css,
// so one theme serves light and dark.
export const prismTheme: PrismTheme = {
  plain: {
    color: 'var(--code-foreground)',
    backgroundColor: 'var(--code)',
  },
  styles: [
    {types: ['comment', 'prolog', 'doctype', 'cdata'], style: {color: 'var(--code-token-comment)'}},
    {types: ['punctuation', 'operator'], style: {color: 'var(--code-token-comment)'}},
    {types: ['atrule', 'keyword', 'selector', 'important'], style: {color: 'var(--code-token-keyword)'}},
    {types: ['string', 'char', 'attr-value', 'inserted', 'builtin'], style: {color: 'var(--code-token-string)'}},
    {types: ['number', 'boolean', 'constant', 'symbol', 'regex', 'variable'], style: {color: 'var(--code-token-number)'}},
    {types: ['function', 'property', 'attr-name'], style: {color: 'var(--code-token-function)'}},
    {types: ['class-name', 'maybe-class-name', 'namespace'], style: {color: 'var(--code-token-type)'}},
    {types: ['tag', 'deleted'], style: {color: 'var(--code-token-tag)'}},
    {types: ['italic'], style: {fontStyle: 'italic'}},
    {types: ['bold'], style: {fontWeight: 'bold'}},
  ],
};
