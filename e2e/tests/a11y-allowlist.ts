// Known accessibility issues the axe scan (26-a11y.spec.ts) tolerates. Only
// critical and serious violations fail the suite, and an entry here is the only way
// to let one pass: each carries the rule, where it applies and the reason. Prefer
// fixing the dashboard; remove an entry as soon as its issue is fixed.
export interface AllowedViolation {
  /** rule is the axe rule id, such as "color-contrast". */
  rule: string;
  /** pages limits the entry to scans whose name starts with one of these (see 26-a11y.spec.ts); omitted means every scan. */
  pages?: string[];
  /** selector is a substring of the violating node's CSS selector; omitted means any node. */
  selector?: string;
  /** reason says why the issue is tolerated and what would fix it. */
  reason: string;
}

// While "Encrypt new backups" is off, the whole method fieldset (#enc-options) is
// dimmed with opacity (.settings-group.is-muted) and its controls are disabled. Those
// components are inactive, which WCAG 1.4.3 exempts from the contrast minimum, but
// axe only recognises disabled form controls, not the labels and hints inside a
// disabled fieldset, so it measures their faded colour. Raising the opacity to
// pass would remove the visual cue that the options are off.
const ENCRYPTION_OFF =
  "Inactive (disabled) components are exempt from the contrast minimum (WCAG 1.4.3); the encryption options are dimmed while encryption is off.";

export const ALLOWED_VIOLATIONS: AllowedViolation[] = [
  { rule: "color-contrast", pages: ["settings encryption"], selector: "#enc-", reason: ENCRYPTION_OFF },
  { rule: "color-contrast", pages: ["settings encryption"], selector: 'label[for="enc-', reason: ENCRYPTION_OFF },
  { rule: "color-contrast", pages: ["settings encryption"], selector: 'data-i18n="enc.', reason: ENCRYPTION_OFF },
  { rule: "color-contrast", pages: ["settings encryption"], selector: 'data-action="generate-key"', reason: ENCRYPTION_OFF },
];
