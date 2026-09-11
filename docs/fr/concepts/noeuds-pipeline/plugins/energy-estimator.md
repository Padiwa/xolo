# energy-estimator

> Plugin livré par défaut, famille « Analyse de la requête ». Retour à la [vue d'ensemble des nœuds](../index.md).

**energy-estimator** estime l'énergie d'une inférence. Ports d'entrée : `input_tokens` (requis), `output_tokens`, `model_params_b` (number). Sorties : `energy_kwh`, `energy_wh`, `duration_ms`, `energy_cost` (number).

`energy_cost` normalise l'énergie entre 0 et 1 sur une échelle logarithmique. L'énergie de référence configurée vaut 0,5. La taille du modèle en milliards de paramètres actifs vient du port ou de la configuration. Branchez `input_tokens` du `request-inspector` et `estimated_output_tokens` du `complexity-scorer`. La méthode de calcul est décrite dans [Estimation énergétique](../../estimation-energetique.md).
