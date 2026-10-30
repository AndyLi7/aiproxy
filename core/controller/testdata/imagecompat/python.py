import base64, os, re, urllib.request
from openai import OpenAI, APIStatusError
base = os.environ['AND76_TEST_BASE']
assert re.fullmatch(r'http://127\.0\.0\.1:\d+/v1', base)
client = OpenAI(base_url=base, api_key='fake-image-sdk-key', max_retries=2)
model = 'openai/gpt-image-1.5/text-to-image'
edit = 'openai/gpt-image-1.5/image-to-image'
png = base64.b64decode(os.environ['AND76_TEST_PNG'])
def key(name): return {'Idempotency-Key':'python-'+name}
def fails(code, fn):
    try: fn()
    except APIStatusError as error:
        assert error.status_code == code, str(error)
        return error
    raise AssertionError('expected status error')
result = client.images.generate(model=model, prompt='draw fixture', extra_headers=key('generate'))
assert base64.b64decode(result.data[0].b64_json) == png
result = client.images.generate(model=model, prompt='draw fixture', response_format='url', extra_headers=key('generate'))
assert urllib.request.urlopen(result.data[0].url).read() == png
result = client.images.edit(model=edit, prompt='edit fixture', image=('image.png', png, 'image/png'), extra_headers=key('edit'))
assert base64.b64decode(result.data[0].b64_json) == png
fails(400, lambda: client.images.generate(model=model, prompt='bad', n=2))
fails(400, lambda: client.images.generate(model=model, prompt='bad', quality='auto'))
fails(400, lambda: client.images.edit(model=edit, prompt='bad', image=[('one.png',png),('two.png',png)]))
fails(409, lambda: client.images.generate(model=model, prompt='changed', extra_headers=key('generate')))
fails(400, lambda: client.images.generate(model=model, prompt='retry', extra_headers={'x-stainless-retry-count':'1'}))
fails(503, lambda: client.images.generate(model=model, prompt='pending', extra_headers=key('pending')))
timed = OpenAI(base_url=base, api_key='fake-image-sdk-key', timeout=.06, max_retries=1)
fails(400, lambda: timed.images.generate(model=model, prompt='slow-no-key'))
result = timed.images.generate(model=model, prompt='slow-key', extra_headers=key('timeout'))
assert result.data[0].b64_json
print('Python SDK: generate/edit/default b64/signed url/strict errors/key conflict/pending/timeout retries passed')
