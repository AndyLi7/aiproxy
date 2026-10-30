import { pathToFileURL } from 'node:url';
import assert from 'node:assert/strict';
const { default: OpenAI, toFile } = await import(pathToFileURL(process.env.AND76_NODE_SDK).href);
const baseURL = process.env.AND76_TEST_BASE;
assert.match(baseURL, /^http:\/\/127\.0\.0\.1:\d+\/v1$/);
const client = new OpenAI({baseURL,apiKey:'fake-image-sdk-key',maxRetries:2});
const model='openai/gpt-image-1.5/text-to-image';
const edit='openai/gpt-image-1.5/image-to-image';
const png=Buffer.from(process.env.AND76_TEST_PNG,'base64');
const key=(id)=>({headers:{'Idempotency-Key':'node-'+id}});
const result=await client.images.generate({model,prompt:'draw fixture'},key('generate'));
assert.deepEqual(Buffer.from(result.data[0].b64_json,'base64'),png);
const url=await client.images.generate({model,prompt:'draw fixture',response_format:'url'},key('generate'));
const download=await fetch(url.data[0].url);assert.equal(download.status,200);assert.deepEqual(Buffer.from(await download.arrayBuffer()),png);
const edited=await client.images.edit({model:edit,prompt:'edit fixture',image:await toFile(png,'image.png')},key('edit'));
assert.deepEqual(Buffer.from(edited.data[0].b64_json,'base64'),png);
for(const input of [{model,prompt:'bad',n:2},{model,prompt:'bad',quality:'auto'},{model,prompt:'bad',response_format:'bad'}]) {
 await assert.rejects(()=>client.images.generate(input),e=>e.status===400);
}
await assert.rejects(async()=>client.images.edit({model:edit,prompt:'bad',image:[await toFile(png,'one.png'),await toFile(png,'two.png')]}),e=>e.status===400);
await assert.rejects(()=>client.images.generate({model,prompt:'different'},key('generate')),e=>e.status===409);
await assert.rejects(()=>client.images.generate({model,prompt:'retry'}, {headers:{'x-stainless-retry-count':'1'}}),e=>e.status===400);
await assert.rejects(()=>client.images.generate({model,prompt:'pending'},key('pending')),e=>e.status===503 && e.code==='image_operation_pending');
const timed=new OpenAI({baseURL,apiKey:'fake-image-sdk-key',timeout:60,maxRetries:1});
await assert.rejects(()=>timed.images.generate({model,prompt:'slow-no-key'}),e=>e.status===400);
const recovered=await timed.images.generate({model,prompt:'slow-key'},key('timeout'));
assert.ok(recovered.data[0].b64_json);
console.log('Node SDK: generate/edit/default b64/signed url/strict errors/key conflict/pending/timeout retries passed');
