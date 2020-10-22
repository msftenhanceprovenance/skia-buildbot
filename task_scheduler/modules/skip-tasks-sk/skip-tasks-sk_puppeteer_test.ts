import * as path from 'path';
import { expect } from 'chai';
import {
  setUpPuppeteerAndDemoPageServer,
  takeScreenshot,
} from '../../../puppeteer-tests/util';

describe('skip-tasks-sk', () => {
  const testBed = setUpPuppeteerAndDemoPageServer(
    path.join(__dirname, '..', '..', 'webpack.config.ts')
  );

  beforeEach(async () => {
    await testBed.page.goto(`${testBed.baseUrl}/dist/skip-tasks-sk.html`);
    await testBed.page.setViewport({ width: 550, height: 550 });
  });

  it('should render the demo page (smoke test)', async () => {
    expect(await testBed.page.$$('skip-tasks-sk')).to.have.length(1);
  });

  describe('screenshots', () => {
    it('starting point', async () => {
      await takeScreenshot(
        testBed.page,
        'task-scheduler',
        'skip-tasks-sk_start'
      );
    });
    it('adds a rule', async () => {
      await testBed.page.click('add-icon-sk');
      await testBed.page.type('#input-name', 'New Rule');
      await testBed.page.type('#input-task-specs input', '.*');
      // TODO(borenet): I would like to use a commit range here, but I was
      // unable to automate the checking of the checkbox and subsequent
      // rendering of the new input field.
      await testBed.page.type('#input-range-start', 'abc123');
      await testBed.page.type(
        '#input-description',
        'This is a detailed description of the rule.'
      );
      await takeScreenshot(
        testBed.page,
        'task-scheduler',
        'skip-tasks-sk_adding-rule'
      );
      await testBed.page.click('#add-button');
      await takeScreenshot(
        testBed.page,
        'task-scheduler',
        'skip-tasks-sk_added-rule'
      );
    });
  });
});
