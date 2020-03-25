/* global describe it web3 */
const assert = require('chai').use(require('chai-as-promised')).assert
// eslint-disable-next-line camelcase
const { recoverTypedSignature_v4 } = require('eth-sig-util')

const RelayRequest = require('../src/js/relayclient/EIP712/RelayRequest')
const Environments = require('../src/js/relayclient/Environments')
const { getEip712Signature } = require('../src/js/relayclient/utils')
const getDataToSign = require('../src/js/relayclient/EIP712/Eip712Helper')

const EIP712Sig = artifacts.require('./EIP712Sig.sol')

contract('Utils', async function (accounts) {
  describe('#getEip712Signature()', async function () {
    it('should generate a valid EIP-712 compatible signature', async function () {
      const chainId = Environments.defEnv.chainId
      const senderAddress = accounts[0]
      const senderNonce = '5'
      const target = accounts[5]
      const encodedFunction = '0xdeadbeef'
      const pctRelayFee = '15'
      const baseRelayFee = '1000'
      const gasPrice = '10000000'
      const gasLimit = '500000'
      const paymaster = accounts[7]
      const relayHub = accounts[8]
      const relayAddress = accounts[9]

      const relayRequest = new RelayRequest({
        senderAddress,
        target,
        encodedFunction,
        gasPrice,
        gasLimit,
        pctRelayFee,
        baseRelayFee,
        senderNonce,
        relayAddress,
        paymaster
      })

      const dataToSign = await getDataToSign({
        chainId,
        relayHub,
        relayRequest
      })
      const sig = await getEip712Signature({
        web3,
        dataToSign
      })

      const recoveredAccount = recoverTypedSignature_v4({
        data: dataToSign,
        sig
      })
      assert.strictEqual(senderAddress.toLowerCase(), recoveredAccount.toLowerCase())

      const eip712Sig = await EIP712Sig.new(relayHub)
      const verify = await eip712Sig.verify(dataToSign.message, sig, { from: senderAddress })
      assert.strictEqual(verify, true)
    })
  })
})
