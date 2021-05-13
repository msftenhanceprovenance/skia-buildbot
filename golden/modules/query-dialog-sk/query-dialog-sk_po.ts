import { PageObject } from '../../../infra-sk/modules/page_object/page_object';
import { ParamSetSkPO } from '../../../infra-sk/modules/paramset-sk/paramset-sk_po';
import { QuerySkPO } from '../../../infra-sk/modules/query-sk/query-sk_po';
import { ParamSet } from 'common-sk/modules/query';
import { PageObjectElement } from '../../../infra-sk/modules/page_object/page_object_element';

/** A page object for the QueryDialogSk component. */
export class QueryDialogSkPO extends PageObject {
  get querySkPO(): Promise<QuerySkPO> {
    return this.poBySelector('query-sk', QuerySkPO);
  }

  get paramSetSkPO(): Promise<ParamSetSkPO> {
    return this.poBySelector('paramset-sk', ParamSetSkPO);
  }

  private get dialog(): Promise<PageObjectElement> {
    return this.selectOnePOE('dialog');
  }

  private get emptySelectionMessage(): Promise<PageObjectElement> {
    return this.selectOnePOE('.empty-selection');
  }

  private get showMatchesBtn(): Promise<PageObjectElement> {
    return this.selectOnePOE('button.show-matches');
  }

  private get cancelBtn(): Promise<PageObjectElement> {
    return this.selectOnePOE('button.cancel');
  }

  async isDialogOpen() {
    return (await this.dialog).applyFnToDOMNode((d) => (d as HTMLDialogElement).open);
  }

  async isEmptySelectionMessageVisible() { return !(await this.emptySelectionMessage).isEmpty(); }

  async isParamSetSkVisible() { return !(await this.paramSetSkPO).isEmpty(); }

  async clickKey(key: string) { await (await this.querySkPO).clickKey(key); }

  async clickValue(value: string) { await (await this.querySkPO).clickValue(value); }

  async clickShowMatchesBtn() { await (await this.showMatchesBtn).click(); }

  async clickCancelBtn() { await (await this.cancelBtn).click(); }

  async getParamSetSkContents() {
    const paramSets = await (await this.paramSetSkPO).getParamSets();
    return paramSets[0]; // There's only one ParamSet.
  }

  /** Returns the key/value pairs available for the user to choose from. */
  async getParamSet() { return (await this.querySkPO).getParamSet(); }

  /** Gets the selected query. */
  async getSelection() { return (await this.querySkPO).getCurrentQuery(); }

  /** Sets the selected query via simulated UI interactions. */
  async setSelection(selection: ParamSet) {
    await (await this.querySkPO).setCurrentQuery(selection);

    // Remove focus from the last selected value in the query-sk component. This reduces flakiness.
    await (await this.dialog).click();
  }
}
