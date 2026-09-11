# fuzzy-evaluator

> Plugin livré par défaut, famille « Décision ». Retour à la [vue d'ensemble des nœuds](../index.md).

**fuzzy-evaluator** applique des règles de logique floue à des nombres. Ses ports d'entrée et de sortie se déclarent dans sa configuration, avec les règles dans un langage dédié. Le pipeline « auto » l'utilise pour combiner complexité, pression budgétaire, coût et sensibilité énergétiques en un `power_level`.

La logique floue donne des transitions douces là où des seuils créent des sauts. Elle demande d'écrire des règles. Pour deux ou trois entrées, `math` puis `compare` suffisent souvent.
