# pseudonymizer

> Plugin livré par défaut, famille « Transformation de la requête ». Retour à la [vue d'ensemble des nœuds](../index.md).

**pseudonymizer** remplace les données personnelles par des pseudonymes avant l'appel au modèle, puis rétablit les valeurs d'origine dans la réponse. Il agit dans les deux sens, ce qui oblige Xolo à attendre la fin de la réponse avant de la renvoyer quand il a effectivement remplacé quelque chose. Il émet un événement quand il détecte une donnée sensible.

Devant un client agentique, il traite aussi les blocs d'outils du format Messages d'Anthropic : le contenu d'un `tool_result` et les arguments d'un `tool_use`. Ce qui apparie un appel à son résultat — `id`, `tool_use_id`, `name` — et les noms d'arguments sont laissés intacts ; un contenu qu'il ne sait pas lire, l'image d'une capture d'écran par exemple, est remplacé par une note et non retiré, sous peine de laisser l'appel correspondant orphelin.

Trois limites à connaître. Les appels d'outils **au format OpenAI** ne sont pas couverts : leurs arguments vivent dans `tool_calls[].function.arguments`, en dehors du contenu du message, et partent donc tels quels. Comme une conversation agentique fait presque toujours remplacer quelque chose, ces échanges perdent le streaming — la réponse n'est renvoyée qu'une fois complète. Enfin, en mode strict avec `verification_on_leak` à `allow`, une fuite détectée fait passer la requête **entière** en passe-plat : l'événement `sensitive-data.leak` le signale, mais du trafic d'outils transporte un fichier là où un message ordinaire transporte une phrase. `block` refuse plutôt que de transmettre.
