# text-classifier

> Plugin livré par défaut, famille « Analyse de la requête ». Retour à la [vue d'ensemble des nœuds](../index.md).

**text-classifier** range la demande dans une catégorie sans appeler de modèle. Sorties : `category` (string), `confidence` (number), `source` (string). Catégories : `analysis`, `code`, `conversation`, `creative`, `factual`, `instruction`, `math`, `rewriting`, `summarization`, `translation`, ou `unknown`.

Des règles lexicales tranchent d'abord les cas explicites, « traduis », « résume », un bloc de code. `source` vaut alors `rule`. Le reste passe par un modèle bayésien embarqué, entraîné sur le corpus du dépôt. Sous la marge minimale configurée, la réponse est `unknown` plutôt qu'une supposition. Le corpus reste petit. Comptez sur les règles pour les catégories nettes, et sur `llm-classifier` quand la précision compte.
