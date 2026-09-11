# budget-pressure

> Plugin livré par défaut, famille « Analyse de la requête ». Retour à la [vue d'ensemble des nœuds](../index.md).

**budget-pressure** mesure la part du budget déjà consommée par l'utilisateur. Sorties : `budget_pressure`, `daily_pressure`, `monthly_pressure`, `yearly_pressure` (number entre 0 et 1), `has_budget` (boolean). Aucune configuration.

`budget_pressure` est la pire des trois périodes. Sans budget configuré, tout vaut 0. Une pression élevée est un bon motif pour rabattre vers un modèle moins cher avant que le quota ne bloque la requête.
