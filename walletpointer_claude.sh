#!/bin/bash
# Run this from the root of the BitTipBot repository.
# It replaces every pattern that assumed Wallet is a pointer (*Wallet)
# with the correct value-type equivalent (Wallet).

set -e
REPO="${1:-.}"

echo "Patching Wallet pointer -> value type in $REPO ..."

# 1. user.Wallet == nil  ->  user.Wallet.ID == ""
find "$REPO" -name "*.go" | xargs sed -i 's/user\.Wallet == nil/user.Wallet.ID == ""/g'

# 2. user.Wallet != nil  ->  user.Wallet.ID != ""
find "$REPO" -name "*.go" | xargs sed -i 's/user\.Wallet != nil/user.Wallet.ID != ""/g'

# 3. toUserDb.Wallet == nil  (in database.go)
find "$REPO" -name "*.go" | xargs sed -i 's/toUserDb\.Wallet == nil/toUserDb.Wallet.ID == ""/g'

# 4. user.Wallet = &lnbits.Wallet{}  ->  user.Wallet = lnbits.Wallet{}
find "$REPO" -name "*.go" | xargs sed -i 's/user\.Wallet = &lnbits\.Wallet{}/user.Wallet = lnbits.Wallet{}/g'

# 5. user.Wallet = &wallet[0]  ->  user.Wallet = wallet[0]
find "$REPO" -name "*.go" | xargs sed -i 's/user\.Wallet = &wallet\[0\]/user.Wallet = wallet[0]/g'

# 6. bot.Client.Info(*user.Wallet)  ->  bot.Client.Info(user.Wallet)
find "$REPO" -name "*.go" | xargs sed -i 's/bot\.Client\.Info(\*user\.Wallet)/bot.Client.Info(user.Wallet)/g'

# 7. bot.Client.Payments(*user.Wallet)  ->  bot.Client.Payments(user.Wallet)
find "$REPO" -name "*.go" | xargs sed -i 's/bot\.Client\.Payments(\*user\.Wallet)/bot.Client.Payments(user.Wallet)/g'

# 8. s.Bot.Client.Payment(*user.Wallet,  ->  s.Bot.Client.Payment(user.Wallet,
find "$REPO" -name "*.go" | xargs sed -i 's/s\.Bot\.Client\.Payment(\*user\.Wallet,/s.Bot.Client.Payment(user.Wallet,/g'

# 9. user.Wallet = &lnbits.Wallet  (in api/lightning.go InvoiceStatus: user.Wallet = &lnbits.Wallet{})
#    already covered by rule 4 above

# 10. InvoiceStatus sets user.Wallet = &lnbits.Wallet{} before calling Payment —
#     after the change it becomes user.Wallet = lnbits.Wallet{} which is a no-op reset, fine.

# 11. Webhook / middleware: w.database.Where("wallet_id = ?") — no change needed, column name unchanged.

echo "Done. Review changes with: git diff"
echo ""
echo "Remaining patterns to check manually:"
grep -rn "\.Wallet == nil\|\.Wallet != nil\|= &lnbits\.Wallet\|= &wallet\[" "$REPO" --include="*.go" || true
