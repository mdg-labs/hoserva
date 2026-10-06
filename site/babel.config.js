// Docusaurus compiles the API theme and plugin (CommonJS) with its Babel
// preset, which would inject ES `import`s for helpers into those CommonJS
// files and make webpack treat them as mixed modules; compile them as scripts.
module.exports = {
  presets: [require.resolve('@docusaurus/core/lib/babel/preset')],
  overrides: [
    {
      test: /node_modules[\\/]docusaurus-(theme|plugin)-openapi-docs[\\/]lib[\\/]/,
      sourceType: 'unambiguous',
    },
  ],
};
