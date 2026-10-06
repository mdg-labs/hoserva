// Adds Tailwind v4's PostCSS plugin to Docusaurus's CSS pipeline, first in the
// chain: Tailwind resolves the @imports and layers in src/css/custom.css
// itself, before Docusaurus's preset-env plugins see them. custom.css leaves
// Tailwind's preflight out so Infima's layout is untouched on the docs pages.
module.exports = function tailwindPlugin() {
  return {
    name: 'tailwind',
    configurePostCss(postcssOptions) {
      postcssOptions.plugins.unshift(require('@tailwindcss/postcss'));
      return postcssOptions;
    },
  };
};
