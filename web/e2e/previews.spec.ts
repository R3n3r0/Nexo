import { uniqueName } from './support/api';
import { expect, test } from './support/fixtures';
import { pdfDocument, pngImage } from './support/samples';

// The headless shell of Chromium has no PDF viewer (a PDF frame is a
// download there): the previews run in the full Chromium, whose headless
// mode embeds the viewer like a desktop browser.
test.use({ channel: 'chromium' });

test.describe('object previews', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-previews');
    await api.createBucket(bucket);
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('TD-E-10 previews an image, a PDF and a text file', async ({ page, api }) => {
    await api.putObject(bucket, 'media/pixel.png', pngImage, 'image/png');
    await api.putObject(bucket, 'media/doc.pdf', pdfDocument, 'application/pdf');
    await api.putObject(
      bucket,
      'media/readme.txt',
      'Plain text preview\nsecond line',
      'text/plain',
    );
    await page.goto(`/buckets/${bucket}/browse/media/`);

    const openPreview = async (name: string) => {
      await page.getByRole('button', { name, exact: true }).click();
      const drawer = page.getByRole('dialog', { name });
      await expect(drawer).toBeVisible();
      await drawer.getByRole('tab', { name: 'Preview' }).click();
      return drawer;
    };
    const close = async () => {
      await page.getByRole('button', { name: 'Close' }).click();
    };

    // Image: decoded by the browser.
    let drawer = await openPreview('pixel.png');
    const image = drawer.getByRole('img', { name: 'Preview of pixel.png' });
    await expect(image).toBeVisible();
    await expect
      .poll(() => image.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth))
      .toBe(8);
    await close();

    // PDF: an inline document the viewer of the browser renders in a frame
    // (regression: a sandboxed frame made Chromium refuse the viewer and
    // show chrome-error://chromewebdata/).
    drawer = await openPreview('doc.pdf');
    const frame = drawer.locator('iframe[title="PDF preview of doc.pdf"]');
    await expect(frame).toBeVisible();
    expect(await frame.getAttribute('sandbox')).toBeNull();
    const src = await frame.getAttribute('src');
    // The frame shows the document, not chrome-error://chromewebdata/.
    const pdfFrame = async () => {
      const handle = await frame.elementHandle();
      return (await handle?.contentFrame()) ?? null;
    };
    await expect.poll(async () => (await pdfFrame())?.url() ?? '').toContain('/objects/preview');
    const viewer = await pdfFrame();
    if (!viewer) throw new Error('the PDF frame has no document');
    expect(viewer.url()).not.toMatch(/^chrome-error:/);
    // The viewer of Chromium embeds the document.
    await expect
      .poll(() => viewer.evaluate(() => document.querySelector('embed')?.type ?? ''))
      .toBe('application/pdf');
    const res = await page.request.get(src ?? '');
    expect(res.status()).toBe(200);
    expect(res.headers()['content-type']).toBe('application/pdf');
    expect(res.headers()['content-disposition']).toMatch(/^inline/);
    expect(Buffer.compare(await res.body(), pdfDocument)).toBe(0);
    await close();

    // Text: shown in the page.
    drawer = await openPreview('readme.txt');
    await expect(drawer.getByText('Plain text preview')).toBeVisible();
    await expect(drawer.getByText('second line')).toBeVisible();
  });
});
