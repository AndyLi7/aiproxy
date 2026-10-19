# Publisher output provenance

These files are outputs of the actual application
`prepareNativeGatewayPublication`, executed locally from exact application commit
`b36c59b4b76cb6ecff65dfc00903e23d6efc9c12`.

- `publisher-ordinary.json`: existing `tests/native-result-publication.test.ts`
  fixture (native image2svg mode and standard frozen prepayment quote).
- `publisher-video.json`: existing `tests/native-video-fixed-hold.test.ts` fixture,
  including `buildNativeMeasuredPricing` and actual publisher call. Public schema
  retains prompt, duration, resolution and audio defaults; measured trial/bill
  dependencies are the existing synthetic fixture functions.
- `publisher-image.json`: cached `tests/fixtures/registry-import/fal-flux-schnell.json`
  document passed through actual `readFalNativeSource`, `buildNativeResultRevision`
  and publisher, with the ordinary test's price/channel fixture. This exercises
  real imported public schemas and safety/sync fixed controls.
- `publisher-tts.json`: existing `speechFixture()` from native-result-publication
  (cached Eleven v4 Turbo source), actual publisher with actual-cost and input-meter
  runtime features, preserving the frozen character quote and meter.
- `publisher-builder.json`: the video fixture with input identity set to the app's
  existing prepare-builder fixture (model `test/native/text-to-video`, physical ID
  11, endpoint `fal-ai/fixture`, synthetic key). Re-executed publisher, not patched
  JSON. The Go test consumes this and exports its actual inventory response when
  explicitly given `B2_INVENTORY_REVIEW_WIRE=/private/tmp/b2-inventory-native-ready.json`.

The source fixture definitions were copied to temporary scripts solely to expose
existing fixture functions; imports resolve to the exact source above. The app
worktree was not edited. Executed with `node --import tsx` from that worktree.
No credentials, provider requests, application DB queries, generation, deployment
or real bucket access were involved. Input keys are named synthetic test values;
none appear in these publisher outputs. SHA256 inventory is recorded below.

- publisher-builder.json: `e6e60e9a039c3e232c7970cfbc191442e98a2efc106041347537617869f6655a`

- publisher-image.json: `f6f712db2a264ece53ca0e329ffd9f4c438ef9d41e4b26f6863bac8a0ff59251`

- publisher-ordinary.json: `43951c6e24f9dcfa0028091133636efd11437a40775cacff7b3c82e1380539e5`

- publisher-tts.json: `62531a5c7beb3c6e571b905537b0cc4c4ca2a1d4b8dec9a46a58d3c91d22aeb3`

- publisher-video.json: `48b8f8c7bc775631e4200c72a35228a2493acb2425bfa0efc7e757a54809f122`
