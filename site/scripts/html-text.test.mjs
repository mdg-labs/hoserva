// escapeText has to match what the site's renderer writes, or check-catalog
// fails the build for a catalog title with a quote in it.
// usage: node --test scripts/html-text.test.mjs   (run from site/, after npm ci)
import assert from 'node:assert/strict';
import test from 'node:test';
import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {escapeText} from './html-text.mjs';

test('escapeText matches the renderer for titles with special characters', () => {
  for (const title of ['Plain', "Let's Chat", 'The "Best" Wiki', 'A & B', '<b>Bold</b>', `Mix's "of" <all> & more`]) {
    const html = renderToStaticMarkup(createElement('h1', null, title));
    assert.equal(html, `<h1>${escapeText(title)}</h1>`);
  }
});
