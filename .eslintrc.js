module.exports = {
  'env': {
    'browser': true,
    'es6': true,
    'jest': true,
    'mocha': true,
    'node': true,
  },
  'globals': {
    'artifacts': false,
    'assert': false,
    'contract': false,
    'web3': false,
  },
  'extends':
    [
      'standard-with-typescript'
    ],
  'parserOptions': {
    'project': './tsconfig.json'
  },
  'rules': {
    'no-console': 'off',
  },
  'overrides': [
    {
      'files': ['*.test.js', '*.test.ts'],
      'rules': {
        'no-unused-expressions': 'off',
        '@typescript-eslint/no-unused-expressions': 'off'
      }
    }
  ]
}
