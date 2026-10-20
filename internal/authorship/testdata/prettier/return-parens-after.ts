export function formatPrice(
  amount: number,
  currency: string,
  locale: string,
  showSymbol: boolean,
): string {
  return new Intl.NumberFormat(locale, {
    style: 'currency',
    currency,
    currencyDisplay: showSymbol ? 'symbol' : 'code',
  }).format(amount);
}
export function buildUserSummary(
  firstName: string,
  lastName: string,
  email: string,
  roles: string[],
  active: boolean,
): string {
  return (
    firstName +
    ' ' +
    lastName +
    ' <' +
    email +
    '> | Roles: ' +
    roles.map((r) => r.toUpperCase()).join(', ') +
    ' | ' +
    (active ? 'Active' : 'Inactive')
  );
}
